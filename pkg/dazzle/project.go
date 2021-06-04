// Copyright © 2020 Christian Weichel

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dazzle

import (
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar"
	"github.com/csweichel/dazzle/pkg/test"
	"github.com/docker/distribution/reference"
	"github.com/minio/highwayhash"
	ignore "github.com/sabhiram/go-gitignore"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// ProjectConfig is the structure of a project's dazzle.yaml
type ProjectConfig struct {
	Combiner struct {
		Combinations []ChunkCombination  `yaml:"combinations"`
		EnvVars      []EnvVarCombination `yaml:"envvars,omitempty"`
	} `yaml:"combiner"`
	ChunkIgnore []string `yaml:"ignore,omitempty"`

	chunkIgnores *ignore.GitIgnore
}

// ChunkCombination combines several chunks to a new image
type ChunkCombination struct {
	Name   string   `yaml:"name"`
	Chunks []string `yaml:"chunks"`
}

// EnvVarCombination describes how env vars are combined
type EnvVarCombination struct {
	Name   string                  `yaml:"name"`
	Action EnvVarCombinationAction `yaml:"action"`
}

// EnvVarCombinationAction defines mode by which an env var is combined
type EnvVarCombinationAction string

const (
	// EnvVarCombineMerge means values are appended with :
	EnvVarCombineMerge EnvVarCombinationAction = "merge"
	// EnvVarCombineMergeUnique is like EnvVarCombineMerge but with unique values only
	EnvVarCombineMergeUnique EnvVarCombinationAction = "merge-unique"
	// EnvVarCombineUseLast means the last value wins
	EnvVarCombineUseLast EnvVarCombinationAction = "use-last"
	// EnvVarCombineUseFirst means the first value wins
	EnvVarCombineUseFirst EnvVarCombinationAction = "use-first"
)

// ChunkConfig configures a chunk
type ChunkConfig struct {
	Variants []ChunkVariant `yaml:"variants"`
}

// ChunkVariant is a variant of a chunk
type ChunkVariant struct {
	Name       string            `yaml:"name"`
	Args       map[string]string `yaml:"args,omitempty"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
}

// Write writes this config as YAML to a file
func (pc *ProjectConfig) Write(dir string) error {
	fd, err := os.OpenFile(filepath.Join(dir, "dazzle.yaml"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer fd.Close()

	err = yaml.NewEncoder(fd).Encode(pc)
	if err != nil {
		return err
	}

	return nil
}

// Project is a dazzle build project
type Project struct {
	Base   ProjectChunk
	Chunks []ProjectChunk
	Config ProjectConfig
}

// ProjectChunk represents a layer chunk in a project
type ProjectChunk struct {
	Name        string
	Dockerfile  []byte
	ContextPath string
	Tests       []*test.Spec
	Args        map[string]string

	cachedHash string
}

// LoadProjectConfig loads a dazzle project config file from disk
func LoadProjectConfig(dir string) (*ProjectConfig, error) {
	var (
		cfg   ProjectConfig
		cfgfn = filepath.Join(dir, "dazzle.yaml")
	)
	fd, err := os.Open(cfgfn)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	err = yaml.NewDecoder(fd).Decode(&cfg)
	fd.Close()
	if err != nil {
		return nil, fmt.Errorf("cannot load config from %s: %w", cfgfn, err)
	}

	cfg.chunkIgnores, err = ignore.CompileIgnoreLines(cfg.ChunkIgnore...)
	if err != nil {
		return nil, fmt.Errorf("cannot load config from %s: %w", cfgfn, err)
	}

	return &cfg, nil
}

// LoadFromDir loads a dazzle project from disk
func LoadFromDir(dir string) (*Project, error) {
	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		return nil, err
	}

	var (
		testsDir  = filepath.Join(dir, "tests")
		chunksDir = filepath.Join(dir, "chunks")
	)

	base, err := loadChunks(dir, testsDir, "base")
	if err != nil {
		return nil, err
	}
	if len(base) != 1 {
		return nil, fmt.Errorf("base must have exactly one variant")
	}

	res := &Project{
		Config: *cfg,
		Base:   base[0],
	}
	chds, err := ioutil.ReadDir(chunksDir)
	if err != nil {
		return nil, err
	}

	res.Chunks = make([]ProjectChunk, 0, len(chds))
	for _, chd := range chds {
		if cfg.chunkIgnores != nil && cfg.chunkIgnores.MatchesPath(chd.Name()) {
			continue
		}
		if strings.HasPrefix(chd.Name(), "_") || strings.HasPrefix(chd.Name(), ".") {
			continue
		}
		if !chd.IsDir() {
			continue
		}
		chunks, err := loadChunks(chunksDir, testsDir, chd.Name())
		if err != nil {
			return nil, err
		}
		res.Chunks = append(res.Chunks, chunks...)
		for _, c := range chunks {
			log.WithField("name", c.Name).WithField("args", c.Args).Info("added chunk to project")
		}
	}

	return res, nil
}

func loadChunks(chunkbase, testbase, name string) (res []ProjectChunk, err error) {
	dir := filepath.Join(chunkbase, name)

	load := func(name string, v ChunkVariant) (ProjectChunk, error) {
		chk := ProjectChunk{
			Name:        name,
			ContextPath: dir,
			Args:        v.Args,
		}

		dfn := "Dockerfile"
		if v.Dockerfile != "" {
			dfn = v.Dockerfile
		}

		var err error
		chk.Dockerfile, err = ioutil.ReadFile(filepath.Join(dir, dfn))
		if err != nil {
			return chk, err
		}

		tf, err := ioutil.ReadFile(filepath.Join(testbase, fmt.Sprintf("%s.yaml", name)))
		if err != nil && !os.IsNotExist(err) {
			return chk, fmt.Errorf("%s: cannot read tests.yaml: %w", dir, err)
		}
		err = yaml.UnmarshalStrict(tf, &chk.Tests)
		if err != nil {
			return chk, fmt.Errorf("%s: cannot read tests.yaml: %w", dir, err)
		}
		return chk, nil
	}

	var (
		cfgfn = filepath.Join(dir, "chunk.yaml")
		cfg   ChunkConfig
	)
	fd, err := os.Open(cfgfn)
	if err == nil {
		defer fd.Close()
		err = yaml.NewDecoder(fd).Decode(&cfg)
		if err != nil {
			return nil, fmt.Errorf("cannot load config from %s: %w", cfgfn, err)
		}
	} else if os.IsNotExist(err) {
		// not a variant chunk
		chk, err := load(name, ChunkVariant{})
		if err != nil {
			return nil, err
		}
		return []ProjectChunk{chk}, nil
	} else {
		return nil, err
	}

	for _, v := range cfg.Variants {
		chk, err := load(fmt.Sprintf("%s:%s", name, v.Name), v)
		if err != nil {
			return nil, err
		}
		res = append(res, chk)
	}

	return res, nil
}

func (p *ProjectChunk) hash(baseref string) (res string, err error) {
	if p.cachedHash != "" {
		return p.cachedHash, nil
	}

	defer func() {
		if err != nil {
			err = fmt.Errorf("cannot compute hash: %w", err)
		}
	}()

	hash, err := highwayhash.New(hashKey)
	if err != nil {
		return
	}

	err = p.manifest(baseref, hash)
	if err != nil {
		return
	}

	res = hex.EncodeToString(hash.Sum(nil))
	p.cachedHash = res
	return
}

func (p *ProjectChunk) manifest(baseref string, out io.Writer) (err error) {
	sources, err := doublestar.Glob(filepath.Join(p.ContextPath, "**/*"))
	if err != nil {
		return
	}

	res := make([]string, 0, len(sources))
	for _, src := range sources {
		if stat, err := os.Stat(src); err != nil {
			return err
		} else if stat.IsDir() {
			res = append(res, strings.TrimPrefix(src, p.ContextPath))
			continue
		}

		file, err := os.OpenFile(src, os.O_RDONLY, 0644)
		if err != nil {
			return err
		}

		hash, err := highwayhash.New(hashKey)
		if err != nil {
			file.Close()
			return err
		}

		_, err = io.Copy(hash, file)
		if err != nil {
			file.Close()
			return err
		}

		err = file.Close()
		if err != nil {
			return err
		}

		res = append(res, fmt.Sprintf("%s:%s", strings.TrimPrefix(src, p.ContextPath), hex.EncodeToString(hash.Sum(nil))))
	}

	if baseref != "" {
		fmt.Fprintf(out, "Baseref: %s\n", baseref)
	}
	fmt.Fprintf(out, "Dockerfile: %s\n", string(p.Dockerfile))
	fmt.Fprintf(out, "Sources:\n%s\n", strings.Join(res, "\n"))
	return nil
}

// ChunkImageType describes the chunk build artifact type
type ChunkImageType string

const (
	// ImageTypeTest is an image built for testing
	ImageTypeTest ChunkImageType = "test"
	// ImageTypeFull is the full chunk image
	ImageTypeFull ChunkImageType = "full"
	// ImageTypeChunked is the chunk image with the base layers removed
	ImageTypeChunked ChunkImageType = "chunked"
	// ImageTypeChunkedNoHash is the chunk image with the base layers removed and no hash in the name
	ImageTypeChunkedNoHash ChunkImageType = "chunked-wohash"
)

// ImageName produces a chunk image name
func (p *ProjectChunk) ImageName(tpe ChunkImageType, sess *BuildSession) (reference.NamedTagged, error) {
	if sess.baseRef == nil {
		return nil, fmt.Errorf("base ref not set")
	}

	if tpe == ImageTypeChunkedNoHash {
		var (
			name = p.Name
			tag  = "latest"
			segs = strings.Split(p.Name, ":")
		)
		if len(segs) == 2 {
			name, tag = segs[0], segs[1]
		}
		dest, err := reference.ParseNamed(fmt.Sprintf("%s/%s", sess.Dest.Name(), name))
		if err != nil {
			return nil, err
		}

		return reference.WithTag(dest, tag)
	}

	safeName := strings.ReplaceAll(p.Name, ":", "-")
	hash, err := p.hash(sess.baseRef.String())
	if err != nil {
		return nil, fmt.Errorf("cannot compute chunk hash: %w", err)
	}
	return reference.WithTag(sess.Dest, fmt.Sprintf("%s--%s--%s", safeName, hash, tpe))
}

// PrintManifest prints the manifest to writer ... this is intended for debugging only
func (p *ProjectChunk) PrintManifest(out io.Writer, sess *BuildSession) error {
	if sess.baseRef == nil {
		return fmt.Errorf("base ref not set")
	}

	return p.manifest(sess.baseRef.String(), out)
}
