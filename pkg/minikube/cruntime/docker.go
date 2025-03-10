/*
Copyright 2019 The Kubernetes Authors All rights reserved.

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

package cruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"
	"time"

	"github.com/blang/semver/v4"
	units "github.com/docker/go-units"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/bootstrapper/images"
	"k8s.io/minikube/pkg/minikube/cni"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/docker"
	"k8s.io/minikube/pkg/minikube/download"
	"k8s.io/minikube/pkg/minikube/image"
	"k8s.io/minikube/pkg/minikube/style"
	"k8s.io/minikube/pkg/minikube/sysinit"
)

// KubernetesContainerPrefix is the prefix of each Kubernetes container
const KubernetesContainerPrefix = "k8s_"

const InternalDockerCRISocket = "/var/run/dockershim.sock"
const ExternalDockerCRISocket = "/var/run/cri-dockerd.sock"

// ErrISOFeature is the error returned when disk image is missing features
type ErrISOFeature struct {
	missing string
}

// NewErrISOFeature creates a new ErrISOFeature
func NewErrISOFeature(missing string) *ErrISOFeature {
	return &ErrISOFeature{
		missing: missing,
	}
}

func (e *ErrISOFeature) Error() string {
	return e.missing
}

// Docker contains Docker runtime state
type Docker struct {
	Socket            string
	Runner            CommandRunner
	ImageRepository   string
	KubernetesVersion semver.Version
	Init              sysinit.Manager
	UseCRI            bool
	CRIService        string
}

// Name is a human readable name for Docker
func (r *Docker) Name() string {
	return "Docker"
}

// Style is the console style for Docker
func (r *Docker) Style() style.Enum {
	return style.Docker
}

// Version retrieves the current version of this runtime
func (r *Docker) Version() (string, error) {
	// Note: the server daemon has to be running, for this call to return successfully
	c := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return "", err
	}
	return strings.Split(rr.Stdout.String(), "\n")[0], nil
}

// SocketPath returns the path to the socket file for Docker
func (r *Docker) SocketPath() string {
	if r.Socket != "" {
		return r.Socket
	}
	return InternalDockerCRISocket
}

// Available returns an error if it is not possible to use this runtime on a host
func (r *Docker) Available() error {
	// If Kubernetes version >= 1.24, require both cri-dockerd and dockerd.
	if r.KubernetesVersion.GTE(semver.Version{Major: 1, Minor: 24}) {
		if _, err := exec.LookPath("cri-dockerd"); err != nil {
			return err
		}
		if _, err := exec.LookPath("dockerd"); err != nil {
			return err
		}
	}
	_, err := exec.LookPath("docker")
	return err
}

// Active returns if docker is active on the host
func (r *Docker) Active() bool {
	return r.Init.Active("docker")
}

// Enable idempotently enables Docker on a host
func (r *Docker) Enable(disOthers, forceSystemd, inUserNamespace bool) error {
	if inUserNamespace {
		return errors.New("inUserNamespace must not be true for docker")
	}

	if disOthers {
		if err := disableOthers(r, r.Runner); err != nil {
			klog.Warningf("disableOthers: %v", err)
		}
	}

	if err := populateCRIConfig(r.Runner, r.SocketPath()); err != nil {
		return err
	}

	if err := r.Init.Unmask("docker.service"); err != nil {
		return err
	}

	if err := r.Init.Enable("docker.socket"); err != nil {
		klog.ErrorS(err, "Failed to enable", "service", "docker.socket")
	}

	if forceSystemd {
		if err := r.forceSystemd(); err != nil {
			return err
		}
	}

	if err := r.Init.Restart("docker"); err != nil {
		return err
	}

	if r.CRIService != "" {
		if err := r.Init.Enable(r.CRIService); err != nil {
			return err
		}
		if err := r.Init.Start(r.CRIService); err != nil {
			return err
		}
	}

	return nil
}

// Restart restarts Docker on a host
func (r *Docker) Restart() error {
	return r.Init.Restart("docker")
}

// Disable idempotently disables Docker on a host
func (r *Docker) Disable() error {
	if r.CRIService != "" {
		if err := r.Init.Stop(r.CRIService); err != nil {
			return err
		}
		if err := r.Init.Disable(r.CRIService); err != nil {
			return err
		}
	}
	klog.Info("disabling docker service ...")
	// because #10373
	if err := r.Init.ForceStop("docker.socket"); err != nil {
		klog.ErrorS(err, "Failed to stop", "service", "docker.socket")
	}
	if err := r.Init.ForceStop("docker.service"); err != nil {
		klog.ErrorS(err, "Failed to stop", "service", "docker.service")
		return err
	}
	if err := r.Init.Disable("docker.socket"); err != nil {
		klog.ErrorS(err, "Failed to disable", "service", "docker.socket")
	}
	return r.Init.Mask("docker.service")
}

// ImageExists checks if image exists based on image name and optionally image sha
func (r *Docker) ImageExists(name string, sha string) bool {
	// expected output looks like [SHA_ALGO:SHA]
	c := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", name)
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return false
	}
	if sha != "" && !strings.Contains(rr.Output(), sha) {
		return false
	}
	return true
}

// ListImages returns a list of images managed by this container runtime
func (r *Docker) ListImages(ListImagesOptions) ([]ListImage, error) {
	c := exec.Command("docker", "images", "--no-trunc", "--format", "{{json .}}")
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return nil, errors.Wrapf(err, "docker images")
	}
	type dockerImage struct {
		ID         string `json:"ID"`
		Repository string `json:"Repository"`
		Tag        string `json:"Tag"`
		Size       string `json:"Size"`
	}
	images := strings.Split(rr.Stdout.String(), "\n")
	result := []ListImage{}
	for _, img := range images {
		if img == "" {
			continue
		}

		var jsonImage dockerImage
		if err := json.Unmarshal([]byte(img), &jsonImage); err != nil {
			return nil, errors.Wrap(err, "Image convert problem")
		}
		size, err := units.FromHumanSize(jsonImage.Size)
		if err != nil {
			return nil, errors.Wrap(err, "Image size convert problem")
		}

		repoTag := fmt.Sprintf("%s:%s", jsonImage.Repository, jsonImage.Tag)
		result = append(result, ListImage{
			ID:          strings.TrimPrefix(jsonImage.ID, "sha256:"),
			RepoDigests: []string{},
			RepoTags:    []string{addDockerIO(repoTag)},
			Size:        fmt.Sprintf("%d", size),
		})
	}
	return result, nil
}

// LoadImage loads an image into this runtime
func (r *Docker) LoadImage(path string) error {
	klog.Infof("Loading image: %s", path)
	c := exec.Command("/bin/bash", "-c", fmt.Sprintf("sudo cat %s | docker load", path))
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "loadimage docker")
	}
	return nil
}

// PullImage pulls an image
func (r *Docker) PullImage(name string) error {
	klog.Infof("Pulling image: %s", name)
	if r.UseCRI {
		return pullCRIImage(r.Runner, name)
	}
	c := exec.Command("docker", "pull", name)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "pull image docker")
	}
	return nil
}

// SaveImage saves an image from this runtime
func (r *Docker) SaveImage(name string, path string) error {
	klog.Infof("Saving image %s: %s", name, path)
	c := exec.Command("/bin/bash", "-c", fmt.Sprintf("docker save '%s' | sudo tee %s >/dev/null", name, path))
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "saveimage docker")
	}
	return nil
}

// RemoveImage removes a image
func (r *Docker) RemoveImage(name string) error {
	klog.Infof("Removing image: %s", name)
	if r.UseCRI {
		return removeCRIImage(r.Runner, name)
	}
	c := exec.Command("docker", "rmi", name)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "remove image docker")
	}
	return nil
}

// TagImage tags an image in this runtime
func (r *Docker) TagImage(source string, target string) error {
	klog.Infof("Tagging image %s: %s", source, target)
	c := exec.Command("docker", "tag", source, target)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "tag image docker")
	}
	return nil
}

// BuildImage builds an image into this runtime
func (r *Docker) BuildImage(src string, file string, tag string, push bool, env []string, opts []string) error {
	klog.Infof("Building image: %s", src)
	args := []string{"build"}
	if file != "" {
		args = append(args, "-f", file)
	}
	if tag != "" {
		args = append(args, "-t", tag)
	}
	args = append(args, src)
	for _, opt := range opts {
		args = append(args, "--"+opt)
	}
	c := exec.Command("docker", args...)
	e := os.Environ()
	e = append(e, env...)
	c.Env = e
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "buildimage docker")
	}
	if tag != "" && push {
		c := exec.Command("docker", "push", tag)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if _, err := r.Runner.RunCmd(c); err != nil {
			return errors.Wrap(err, "pushimage docker")
		}
	}
	return nil
}

// PushImage pushes an image
func (r *Docker) PushImage(name string) error {
	klog.Infof("Pushing image: %s", name)
	c := exec.Command("docker", "push", name)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "push image docker")
	}
	return nil
}

// CGroupDriver returns cgroup driver ("cgroupfs" or "systemd")
func (r *Docker) CGroupDriver() (string, error) {
	// Note: the server daemon has to be running, for this call to return successfully
	c := exec.Command("docker", "info", "--format", "{{.CgroupDriver}}")
	rr, err := r.Runner.RunCmd(c)
	if err != nil {
		return "", err
	}
	return strings.Split(rr.Stdout.String(), "\n")[0], nil
}

// KubeletOptions returns kubelet options for a runtime.
func (r *Docker) KubeletOptions() map[string]string {
	if r.UseCRI {
		return map[string]string{
			"container-runtime":          "remote",
			"container-runtime-endpoint": r.SocketPath(),
			"image-service-endpoint":     r.SocketPath(),
			"runtime-request-timeout":    "15m",
		}
	}
	return map[string]string{
		"container-runtime": "docker",
	}
}

// ListContainers returns a list of containers
func (r *Docker) ListContainers(o ListContainersOptions) ([]string, error) {
	if r.UseCRI {
		return listCRIContainers(r.Runner, "", o)
	}
	args := []string{"ps"}
	switch o.State {
	case All:
		args = append(args, "-a")
	case Running:
		args = append(args, "--filter", "status=running")
	case Paused:
		args = append(args, "--filter", "status=paused")
	}

	nameFilter := KubernetesContainerPrefix + o.Name
	if len(o.Namespaces) > 0 {
		// Example result: k8s.*(kube-system|kubernetes-dashboard)
		nameFilter = fmt.Sprintf("%s.*_(%s)_", nameFilter, strings.Join(o.Namespaces, "|"))
	}

	args = append(args, fmt.Sprintf("--filter=name=%s", nameFilter), "--format={{.ID}}")
	rr, err := r.Runner.RunCmd(exec.Command("docker", args...))
	if err != nil {
		return nil, errors.Wrapf(err, "docker")
	}
	var ids []string
	for _, line := range strings.Split(rr.Stdout.String(), "\n") {
		if line != "" {
			ids = append(ids, line)
		}
	}
	return ids, nil
}

// KillContainers forcibly removes a running container based on ID
func (r *Docker) KillContainers(ids []string) error {
	if r.UseCRI {
		return killCRIContainers(r.Runner, ids)
	}
	if len(ids) == 0 {
		return nil
	}
	klog.Infof("Killing containers: %s", ids)
	args := append([]string{"rm", "-f"}, ids...)
	c := exec.Command("docker", args...)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "killing containers docker")
	}
	return nil
}

// StopContainers stops a running container based on ID
func (r *Docker) StopContainers(ids []string) error {
	if r.UseCRI {
		return stopCRIContainers(r.Runner, ids)
	}
	if len(ids) == 0 {
		return nil
	}
	klog.Infof("Stopping containers: %s", ids)
	args := append([]string{"stop"}, ids...)
	c := exec.Command("docker", args...)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "docker")
	}
	return nil
}

// PauseContainers pauses a running container based on ID
func (r *Docker) PauseContainers(ids []string) error {
	if r.UseCRI {
		return pauseCRIContainers(r.Runner, "", ids)
	}
	if len(ids) == 0 {
		return nil
	}
	klog.Infof("Pausing containers: %s", ids)
	args := append([]string{"pause"}, ids...)
	c := exec.Command("docker", args...)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "docker")
	}
	return nil
}

// UnpauseContainers unpauses a container based on ID
func (r *Docker) UnpauseContainers(ids []string) error {
	if r.UseCRI {
		return unpauseCRIContainers(r.Runner, "", ids)
	}
	if len(ids) == 0 {
		return nil
	}
	klog.Infof("Unpausing containers: %s", ids)
	args := append([]string{"unpause"}, ids...)
	c := exec.Command("docker", args...)
	if _, err := r.Runner.RunCmd(c); err != nil {
		return errors.Wrap(err, "docker")
	}
	return nil
}

// ContainerLogCmd returns the command to retrieve the log for a container based on ID
func (r *Docker) ContainerLogCmd(id string, len int, follow bool) string {
	if r.UseCRI {
		return criContainerLogCmd(r.Runner, id, len, follow)
	}
	var cmd strings.Builder
	cmd.WriteString("docker logs ")
	if len > 0 {
		cmd.WriteString(fmt.Sprintf("--tail %d ", len))
	}
	if follow {
		cmd.WriteString("--follow ")
	}

	cmd.WriteString(id)
	return cmd.String()
}

// SystemLogCmd returns the command to retrieve system logs
func (r *Docker) SystemLogCmd(len int) string {
	return fmt.Sprintf("sudo journalctl -u docker -n %d", len)
}

// ForceSystemd forces the docker daemon to use systemd as cgroup manager
func (r *Docker) forceSystemd() error {
	klog.Infof("Forcing docker to use systemd as cgroup manager...")
	daemonConfig := `{
"exec-opts": ["native.cgroupdriver=systemd"],
"log-driver": "json-file",
"log-opts": {
	"max-size": "100m"
},
"storage-driver": "overlay2"
}
`
	ma := assets.NewMemoryAsset([]byte(daemonConfig), "/etc/docker", "daemon.json", "0644")
	return r.Runner.Copy(ma)
}

// Preload preloads docker with k8s images:
// 1. Copy over the preloaded tarball into the VM
// 2. Extract the preloaded tarball to the correct directory
// 3. Remove the tarball within the VM
func (r *Docker) Preload(cc config.ClusterConfig) error {
	if !download.PreloadExists(cc.KubernetesConfig.KubernetesVersion, cc.KubernetesConfig.ContainerRuntime, cc.Driver) {
		return nil
	}
	k8sVersion := cc.KubernetesConfig.KubernetesVersion
	cRuntime := cc.KubernetesConfig.ContainerRuntime

	// If images already exist, return
	images, err := images.Kubeadm(cc.KubernetesConfig.ImageRepository, k8sVersion)
	if err != nil {
		return errors.Wrap(err, "getting images")
	}
	if dockerImagesPreloaded(r.Runner, images) {
		klog.Info("Images already preloaded, skipping extraction")
		return nil
	}

	refStore := docker.NewStorage(r.Runner)
	if err := refStore.Save(); err != nil {
		klog.Infof("error saving reference store: %v", err)
	}

	tarballPath := download.TarballPath(k8sVersion, cRuntime)
	targetDir := "/"
	targetName := "preloaded.tar.lz4"
	dest := path.Join(targetDir, targetName)

	c := exec.Command("which", "lz4")
	if _, err := r.Runner.RunCmd(c); err != nil {
		return NewErrISOFeature("lz4")
	}

	// Copy over tarball into host
	fa, err := assets.NewFileAsset(tarballPath, targetDir, targetName, "0644")
	if err != nil {
		return errors.Wrap(err, "getting file asset")
	}
	defer func() {
		if err := fa.Close(); err != nil {
			klog.Warningf("error closing the file %s: %v", fa.GetSourcePath(), err)
		}
	}()

	t := time.Now()
	if err := r.Runner.Copy(fa); err != nil {
		return errors.Wrap(err, "copying file")
	}
	klog.Infof("Took %f seconds to copy over tarball", time.Since(t).Seconds())

	// extract the tarball to /var in the VM
	if rr, err := r.Runner.RunCmd(exec.Command("sudo", "tar", "-I", "lz4", "-C", "/var", "-xf", dest)); err != nil {
		return errors.Wrapf(err, "extracting tarball: %s", rr.Output())
	}

	//  remove the tarball in the VM
	if err := r.Runner.Remove(fa); err != nil {
		klog.Infof("error removing tarball: %v", err)
	}

	// save new reference store again
	if err := refStore.Save(); err != nil {
		klog.Infof("error saving reference store: %v", err)
	}
	// update reference store
	if err := refStore.Update(); err != nil {
		klog.Infof("error updating reference store: %v", err)
	}
	return r.Restart()
}

// dockerImagesPreloaded returns true if all images have been preloaded
func dockerImagesPreloaded(runner command.Runner, images []string) bool {
	rr, err := runner.RunCmd(exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}"))
	if err != nil {
		return false
	}
	preloadedImages := map[string]struct{}{}
	for _, i := range strings.Split(rr.Stdout.String(), "\n") {
		i = image.TrimDockerIO(i)
		preloadedImages[i] = struct{}{}
	}

	klog.Infof("Got preloaded images: %s", rr.Output())

	// Make sure images == imgs
	for _, i := range images {
		i = image.TrimDockerIO(i)
		if _, ok := preloadedImages[i]; !ok {
			klog.Infof("%s wasn't preloaded", i)
			return false
		}
	}
	return true
}

// Add docker.io prefix
func addDockerIO(name string) string {
	var reg, usr, img string
	p := strings.SplitN(name, "/", 2)
	if len(p) > 1 && strings.Contains(p[0], ".") {
		reg = p[0]
		img = p[1]
	} else {
		reg = "docker.io"
		img = name
		p = strings.SplitN(img, "/", 2)
		if len(p) > 1 {
			usr = p[0]
			img = p[1]
		} else {
			usr = "library"
			img = name
		}
		return reg + "/" + usr + "/" + img
	}
	return reg + "/" + img
}

func dockerBoundToContainerd(runner command.Runner) bool {
	// NOTE: assumes systemd
	rr, err := runner.RunCmd(exec.Command("sudo", "systemctl", "cat", "docker.service"))
	if err != nil {
		klog.Warningf("unable to check if docker is bound to containerd")
		return false
	}

	if strings.Contains(rr.Stdout.String(), "\nBindsTo=containerd") {
		return true
	}

	return false
}

// ImagesPreloaded returns true if all images have been preloaded
func (r *Docker) ImagesPreloaded(images []string) bool {
	return dockerImagesPreloaded(r.Runner, images)
}

const (
	CNIBinDir   = "/opt/cni/bin"
	CNIConfDir  = "/etc/cni/net.d"
	CNICacheDir = "/var/lib/cni/cache"
)

func dockerConfigureNetworkPlugin(r Docker, cr CommandRunner, networkPlugin string) error {
	if networkPlugin == "" {
		// no-op plugin
		return nil
	}

	args := ""
	if networkPlugin == "cni" {
		args += " --cni-bin-dir=" + CNIBinDir
		args += " --cni-cache-dir=" + CNICacheDir
		args += " --cni-conf-dir=" + cni.ConfDir
		args += " --hairpin-mode=promiscuous-bridge"
	}

	opts := struct {
		NetworkPlugin  string
		ExtraArguments string
	}{
		NetworkPlugin:  networkPlugin,
		ExtraArguments: args,
	}

	const CRIDockerServiceConfFile = "/etc/systemd/system/cri-docker.service.d/10-cni.conf"
	var CRIDockerServiceConfTemplate = template.Must(template.New("criDockerServiceConfTemplate").Parse(`[Service]
ExecStart=
ExecStart=/usr/bin/cri-dockerd --container-runtime-endpoint fd:// --network-plugin={{.NetworkPlugin}}{{.ExtraArguments}}`))

	b := bytes.Buffer{}
	if err := CRIDockerServiceConfTemplate.Execute(&b, opts); err != nil {
		return errors.Wrap(err, "failed to execute template")
	}
	criDockerService := b.Bytes()
	c := exec.Command("sudo", "mkdir", "-p", path.Dir(CRIDockerServiceConfFile))
	if _, err := cr.RunCmd(c); err != nil {
		return errors.Wrapf(err, "failed to create directory")
	}
	svc := assets.NewMemoryAssetTarget(criDockerService, CRIDockerServiceConfFile, "0644")
	if err := cr.Copy(svc); err != nil {
		return errors.Wrap(err, "failed to copy template")
	}
	return r.Init.Restart("cri-docker")
}
