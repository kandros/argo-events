/*
Copyright 2018 BlackRock, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package artifacts

import (
	"fmt"
	"io/ioutil"
	"os"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	go_git_ssh "gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"

	"github.com/argoproj/argo-events/common"
	"github.com/argoproj/argo-events/pkg/apis/sensor/v1alpha1"
)

const (
	DefaultRemote = "origin"
	DefaultBranch = "master"
)

var (
	fetchRefSpec = []config.RefSpec{
		"refs/*:refs/*",
		"HEAD:refs/heads/HEAD",
	}
)

type GitArtifactReader struct {
	artifact *v1alpha1.GitArtifact
}

// NewGitReader returns a new git reader
func NewGitReader(gitArtifact *v1alpha1.GitArtifact) (*GitArtifactReader, error) {
	return &GitArtifactReader{
		artifact: gitArtifact,
	}, nil
}

func (g *GitArtifactReader) getRemote() string {
	if g.artifact.Remote != nil {
		return g.artifact.Remote.Name
	}
	return DefaultRemote
}

func getSSHKeyAuth(sshKeyFile string) (transport.AuthMethod, error) {
	sshKey, err := ioutil.ReadFile(sshKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read ssh key file. err: %+v", err)
	}
	signer, err := ssh.ParsePrivateKey(sshKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ssh key. err: %+v", err)
	}
	auth := &go_git_ssh.PublicKeys{User: "git", Signer: signer}
	auth.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	return auth, nil
}

func (g *GitArtifactReader) getGitAuth() (transport.AuthMethod, error) {
	if g.artifact.Creds != nil {
		username, err := common.GetSecretFromVolume(g.artifact.Creds.Username)
		if err != nil {
			return nil, errors.Wrap(err, "failed to retrieve username")
		}
		password, err := common.GetSecretFromVolume(g.artifact.Creds.Password)
		if err != nil {
			return nil, errors.Wrap(err, "failed to retrieve password")
		}
		return &http.BasicAuth{
			Username: username,
			Password: password,
		}, nil
	}
	if g.artifact.SSHKeySecret != nil {
		sshKeyPath, err := common.GetSecretVolumePath(g.artifact.SSHKeySecret)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get SSH key from mounted volume")
		}
		return getSSHKeyAuth(sshKeyPath)
	}
	// DEPRECATED
	if g.artifact.DeprecatedSSHKeyPath != "" {
		return getSSHKeyAuth(g.artifact.DeprecatedSSHKeyPath)
	}
	return nil, nil
}

func (g *GitArtifactReader) readFromRepository(r *git.Repository, dir string) ([]byte, error) {
	auth, err := g.getGitAuth()
	if err != nil {
		return nil, err
	}

	if g.artifact.Remote != nil {
		_, err := r.CreateRemote(&config.RemoteConfig{
			Name: g.artifact.Remote.Name,
			URLs: g.artifact.Remote.URLS,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create remote. err: %+v", err)
		}

		fetchOptions := &git.FetchOptions{
			RemoteName: g.artifact.Remote.Name,
			RefSpecs:   fetchRefSpec,
			Force:      true,
		}
		if auth != nil {
			fetchOptions.Auth = auth
		}

		if err := r.Fetch(fetchOptions); err != nil {
			return nil, fmt.Errorf("failed to fetch remote %s. err: %+v", g.artifact.Remote.Name, err)
		}
	}

	w, err := r.Worktree()
	if err != nil {
		return nil, fmt.Errorf("failed to get working tree. err: %+v", err)
	}

	fetchOptions := &git.FetchOptions{
		RemoteName: g.getRemote(),
		RefSpecs:   fetchRefSpec,
		Force:      true,
	}
	if auth != nil {
		fetchOptions.Auth = auth
	}

	// In the case of a specific given ref, it isn't necessary to fetch anything
	// but the single ref
	if g.artifact.Ref != "" {
		fetchOptions.Depth = 1
		fetchOptions.RefSpecs = []config.RefSpec{config.RefSpec(g.artifact.Ref + ":" + g.artifact.Ref)}
	}

	if err := r.Fetch(fetchOptions); err != nil && err != git.NoErrAlreadyUpToDate {
		return nil, fmt.Errorf("failed to fetch. err: %v", err)
	}

	if err := w.Checkout(g.getBranchOrTag()); err != nil {
		return nil, fmt.Errorf("failed to checkout. err: %+v", err)
	}

	// In the case of a specific given ref, it shouldn't be necessary to pull
	if g.artifact.Ref != "" {
		pullOpts := &git.PullOptions{
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
			ReferenceName:     g.getBranchOrTag().Branch,
			Force:             true,
		}
		if auth != nil {
			pullOpts.Auth = auth
		}

		if err := w.Pull(pullOpts); err != nil && err != git.NoErrAlreadyUpToDate {
			return nil, fmt.Errorf("failed to pull latest updates. err: %+v", err)
		}
	}

	return ioutil.ReadFile(fmt.Sprintf("%s/%s", dir, g.artifact.FilePath))
}

func (g *GitArtifactReader) getBranchOrTag() *git.CheckoutOptions {
	opts := &git.CheckoutOptions{}

	opts.Branch = plumbing.NewBranchReferenceName(DefaultBranch)

	if g.artifact.Branch != "" {
		opts.Branch = plumbing.NewBranchReferenceName(g.artifact.Branch)
	}
	if g.artifact.Tag != "" {
		opts.Branch = plumbing.NewTagReferenceName(g.artifact.Tag)
	}
	if g.artifact.Ref != "" {
		opts.Branch = plumbing.ReferenceName(g.artifact.Ref)
	}

	return opts
}

func (g *GitArtifactReader) Read() ([]byte, error) {
	cloneDir := g.artifact.CloneDirectory
	if cloneDir == "" {
		tempDir, err := ioutil.TempDir("", "git-tmp")
		if err != nil {
			return nil, errors.Wrap(err, "failed to create a temp file to clone the repository")
		}
		defer os.Remove(tempDir)
		cloneDir = tempDir
	}

	r, err := git.PlainOpen(cloneDir)
	if err != nil {
		if err != git.ErrRepositoryNotExists {
			return nil, fmt.Errorf("failed to open repository. err: %+v", err)
		}

		cloneOpt := &git.CloneOptions{
			URL:               g.artifact.URL,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		}

		auth, err := g.getGitAuth()
		if err != nil {
			return nil, err
		}
		if auth != nil {
			cloneOpt.Auth = auth
		}

		// In the case of a specific given ref, it isn't necessary to have branch
		// histories
		if g.artifact.Ref != "" {
			cloneOpt.Depth = 1
		}

		r, err = git.PlainClone(cloneDir, false, cloneOpt)
		if err != nil {
			return nil, fmt.Errorf("failed to clone repository. err: %+v", err)
		}
	}
	return g.readFromRepository(r, cloneDir)
}
