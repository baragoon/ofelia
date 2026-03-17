package core

import (
	"archive/tar"
	"bytes"
	"os"

	docker "github.com/fsouza/go-dockerclient"
)

func BuildTestImage(client *docker.Client, name string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dockerfile := []byte("FROM alpine\n")
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(dockerfile))}); err != nil {
		return err
	}
	if _, err := tw.Write(dockerfile); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
		       options := docker.BuildImageOptions{
			       Name:         name,
			       InputStream:  &buf,
			       OutputStream: os.Stdout,
		       }
		       return client.BuildImage(options)
}
