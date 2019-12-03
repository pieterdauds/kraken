// Copyright (c) 2016-2019 Uber Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package dockerdaemon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context/ctxhttp"
)

const _defaultTimeout = 32 * time.Second

// DockerClient is a docker daemon client.
type DockerClient interface {
	ImagePull(ctx context.Context, repo, tag string) error
}

type dockerClient struct {
	version  string
	scheme   string
	addr     string
	basePath string
	registry string

	client *http.Client
}

// NewDockerClient creates a new DockerClient.
func NewDockerClient(host, scheme, version, registry string) (DockerClient, error) {
	client, addr, basePath, err := parseHost(host)
	if err != nil {
		return nil, fmt.Errorf("parse docker host `%s`: %s", host, err)
	}

	return &dockerClient{
		version:  version,
		scheme:   scheme,
		addr:     addr,
		basePath: basePath,
		registry: registry,
		client:   client,
	}, nil
}

// parseHost parse host URL and returns a HTTP client.
// This is needed because url.Parse cannot correctly parse url of format
// "unix:///...".
func parseHost(host string) (*http.Client, string, string, error) {
	strs := strings.SplitN(host, "://", 2)
	if len(strs) == 1 {
		return nil, "", "", fmt.Errorf("unable to parse docker host `%s`", host)
	}

	var basePath string
	transport := new(http.Transport)

	protocol, addr := strs[0], strs[1]
	if protocol == "tcp" {
		parsed, err := url.Parse("tcp://" + addr)
		if err != nil {
			return nil, "", "", err
		}
		addr = parsed.Host
		basePath = parsed.Path
	} else if protocol == "unix" {
		if len(addr) > len(syscall.RawSockaddrUnix{}.Path) {
			return nil, "", "", fmt.Errorf("Unix socket path %q is too long", addr)
		}
		transport.DisableCompression = true
		transport.Dial = func(_, _ string) (net.Conn, error) {
			return net.DialTimeout(protocol, addr, _defaultTimeout)
		}
	} else {
		return nil, "", "", fmt.Errorf("Protocol %s not supported", protocol)
	}

	client := &http.Client{
		Transport: transport,
	}
	return client, addr, basePath, nil
}

// ImagePull calls `docker pull` on an image from known registry.
func (cli *dockerClient) ImagePull(ctx context.Context, repo, tag string) error {
	v := url.Values{}
	fromImage := fmt.Sprintf("%s/%s", cli.registry, repo)
	v.Set("fromImage", fromImage)
	v.Set("tag", tag)
	headers := map[string][]string{"X-Registry-Auth": {""}}
	return cli.post(ctx, "/images/create", v, headers, nil, true)
}

func (cli *dockerClient) post(
	ctx context.Context, urlPath string, query url.Values, header http.Header,
	body io.Reader, streamRespBody bool) error {

	// Construct request. It veries depending on client version.
	var apiPath string
	if cli.version != "" {
		v := strings.TrimPrefix(cli.version, "v")
		apiPath = fmt.Sprintf("%s/v%s%s", cli.basePath, v, urlPath)
	} else {
		apiPath = fmt.Sprintf("%s%s", cli.basePath, urlPath)
	}
	u := &url.URL{Path: apiPath}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	if body == nil {
		body = bytes.NewReader([]byte{})
	}
	req, err := http.NewRequest("POST", u.String(), body)
	if err != nil {
		return fmt.Errorf("create request: %s", err)
	}
	req.Header = header
	req.Host = "docker"
	req.URL.Host = cli.addr
	req.URL.Scheme = cli.scheme

	resp, err := ctxhttp.Do(ctx, cli.client, req)
	if err != nil {
		return fmt.Errorf("send post request: %s", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errMsg, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("read error resp: %s", err)
		}
		return fmt.Errorf("Error posting to %s: code %d, err: %s", urlPath, resp.StatusCode, errMsg)
	}

	// Docker daemon returns 200 early. Close resp.Body after reading all.
	if streamRespBody {
		if _, err := ioutil.ReadAll(resp.Body); err != nil {
			return fmt.Errorf("read resp body: %s", err)
		}
	}

	return nil
}
