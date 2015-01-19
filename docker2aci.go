package main

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/appc/spec/aci"
	"github.com/appc/spec/pkg/tarheader"
	"github.com/appc/spec/schema"
	"github.com/appc/spec/schema/types"
	"github.com/coreos/rocket/cas"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/runconfig"
)

type DockerImageData struct {
	ID            string            `json:"id"`
	Parent        string            `json:"parent,omitempty"`
	Comment       string            `json:"comment,omitempty"`
	Created       time.Time         `json:"created"`
	Container     string            `json:"container,omitempty"`
	DockerVersion string            `json:"docker_version,omitempty"`
	Author        string            `json:"author,omitempty"`
	Config        *runconfig.Config `json:"config,omitempty"`
	Architecture  string            `json:"architecture,omitempty"`
	OS            string            `json:"os,omitempty"`
	Checksum      string            `json:"checksum"`
}

type RepoData struct {
	Tokens    []string
	Endpoints []string
}

type DockerURL struct {
	IndexURL  string
	ImageName string
	Tag       string
}

const (
	defaultIndex  = "index.docker.io"
	defaultTag    = "latest"
	rocketDir     = "/var/lib/rkt"
	schemaVersion = "0.1.1"
)

func makeEndpointsList(headers []string) []string {
	var endpoints []string

	for _, ep := range headers {
		endpointsList := strings.Split(ep, ",")
		for _, endpointEl := range endpointsList {
			endpoints = append(
				endpoints,
				// TODO(iaguis) discover if httpsOrHTTP
				fmt.Sprintf("https://%s/v1/", strings.TrimSpace(endpointEl)))
		}
	}

	return endpoints
}

func GetRepoData(indexURL string, remote string) (*RepoData, error) {
	client := &http.Client{}
	repositoryURL := fmt.Sprintf("%s/%s/v1/%s/%s/images", "https:/", indexURL, "repositories", remote)

	req, err := http.NewRequest("GET", repositoryURL, nil)
	if err != nil {
		return nil, err
	}

	// TODO(iaguis) add auth?
	req.Header.Set("X-Docker-Token", "true")

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP code: %d, URL: %s", res.StatusCode, req.URL)
	}

	var tokens []string
	if res.Header.Get("X-Docker-Token") != "" {
		tokens = res.Header["X-Docker-Token"]
	}

	var endpoints []string
	if res.Header.Get("X-Docker-Endpoints") != "" {
		endpoints = makeEndpointsList(res.Header["X-Docker-Endpoints"])
	} else {
		// Assume same endpoint
		endpoints = append(endpoints, indexURL)
	}

	return &RepoData{
		Endpoints: endpoints,
		Tokens:    tokens,
	}, nil
}

func GetRemoteImageJSON(imgID, registry string, token []string) ([]byte, int, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", registry+"images/"+imgID+"/json", nil)
	if err != nil {
		return nil, -1, err
	}
	setAuthToken(req, token)
	res, err := client.Do(req)
	if err != nil {
		return nil, -1, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, -1, fmt.Errorf("HTTP code: %d, URL: %s", res.StatusCode, req.URL)
	}

	imageSize := -1

	if hdr := res.Header.Get("X-Docker-Size"); hdr != "" {
		imageSize, err = strconv.Atoi(hdr)
		if err != nil {
			return nil, -1, err
		}
	}

	jsonString, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, -1, fmt.Errorf("Failed to read downloaded json: %v (%s)", err, jsonString)
	}

	return jsonString, imageSize, nil
}

func GetRemoteLayer(imgID, registry string, token []string, imgSize int64) (io.ReadCloser, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", registry+"images/"+imgID+"/layer", nil)
	if err != nil {
		return nil, err
	}

	setAuthToken(req, token)

	fmt.Printf("%s: Downloading layer\n", imgID)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("HTTP code: %d. URL: %s", res.StatusCode, req.URL)
	}

	return res.Body, nil
}

func GetImageIDFromTag(registry string, appName string, tag string, token []string) (string, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", registry+"repositories/"+appName+"/tags/"+tag, nil)
	if err != nil {
		return "", fmt.Errorf("Failed to get Image ID: %s, URL: %s", err, req.URL)
	}

	setAuthToken(req, token)
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Failed to get Image ID: %s, URL: %s", err, req.URL)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return "", fmt.Errorf("HTTP code: %d. URL: %s", res.StatusCode, req.URL)
	}

	jsonString, err := ioutil.ReadAll(res.Body)

	if err != nil {
		return "", err
	}

	var imageID string

	if err := json.Unmarshal(jsonString, &imageID); err != nil {
		return "", fmt.Errorf("Error unmarshaling: %v", err)
	}

	return imageID, nil
}

func GetAncestry(imgID, registry string, token []string) ([]string, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", registry+"images/"+imgID+"/ancestry", nil)
	if err != nil {
		return nil, err
	}

	setAuthToken(req, token)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP code: %d. URL: %s", res.StatusCode, req.URL)
	}

	var ancestry []string

	jsonString, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("Failed to read downloaded json: %s (%s)", err, jsonString)
	}

	if err := json.Unmarshal(jsonString, &ancestry); err != nil {
		return nil, fmt.Errorf("Error unmarshaling: %v", err)
	}

	return ancestry, nil
}

func setAuthToken(req *http.Request, token []string) {
	if req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Token "+strings.Join(token, ","))
	}
}

func GenerateManifest(layerData DockerImageData, dockerURL *DockerURL, parentImageID string) (*schema.ImageManifest, error) {
	dockerConfig := layerData.Config
	genManifest := &schema.ImageManifest{}

	appURL := dockerURL.IndexURL + "/" + dockerURL.ImageName
	name, err := types.NewACName(appURL)
	if err != nil {
		return nil, err
	}
	genManifest.Name = *name

	acVersion, _ := types.NewSemVer(schemaVersion)
	genManifest.ACVersion = *acVersion

	genManifest.ACKind = types.ACKind("ImageManifest")

	var labels types.Labels

	layer, _ := types.NewACName("layer")
	labels = append(labels, types.Label{Name: *layer, Value: layerData.ID})

	tag := dockerURL.Tag
	version, _ := types.NewACName("version")
	labels = append(labels, types.Label{Name: *version, Value: tag})

	genManifest.Labels = labels

	if dockerConfig != nil {
		if len(dockerConfig.Cmd) > 0 {
			exec := types.Exec(dockerConfig.Cmd)
			// TODO(iaguis) populate user and group
			app := &types.App{Exec: exec, User: "0", Group: "0"}
			genManifest.App = app
		}
	}

	if parentImageID != "" {
		var dependencies types.Dependencies
		hash, err := types.NewHash(parentImageID)
		if err != nil {
			return nil, err
		}

		dependencies = append(dependencies, types.Dependency{App: *name, ImageID: hash})
		genManifest.Dependencies = dependencies
	}

	return genManifest, nil
}

func parseArgument(arg string) *DockerURL {
	indexURL := defaultIndex
	tag := defaultTag

	argParts := strings.SplitN(arg, "/", 2)
	var appString string
	if len(argParts) > 1 {
		if strings.Index(argParts[0], ".") != -1 {
			indexURL = argParts[0]
			appString = argParts[1]
		} else {
			appString = strings.Join(argParts, "/")
		}
	} else {
		appString = argParts[0]
	}

	imageName := appString
	appParts := strings.Split(appString, ":")

	if len(appParts) > 1 {
		tag = appParts[len(appParts)-1]
		imageNameParts := appParts[0 : len(appParts)-1]
		imageName = strings.Join(imageNameParts, ":")
	}

	return &DockerURL{
		IndexURL:  indexURL,
		ImageName: imageName,
		Tag:       tag,
	}
}

// shamelessly copied from actool
func buildWalker(root string, aw aci.ArchiveWriter) filepath.WalkFunc {
	// cache of inode -> filepath, used to leverage hard links in the archive
	inos := map[uint64]string{}
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relpath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relpath == "." {
			return nil
		}
		if relpath == aci.ManifestFile {
			// ignore; this will be written by the archive writer
			// TODO(jonboulle): does this make sense? maybe just remove from archivewriter?
			return nil
		}

		link := ""
		var r io.Reader
		switch info.Mode() & os.ModeType {
		case os.ModeCharDevice:
		case os.ModeDevice:
		case os.ModeDir:
		case os.ModeSymlink:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			link = target
		default:
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			r = file
		}

		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			panic(err)
		}
		// Because os.FileInfo's Name method returns only the base
		// name of the file it describes, it may be necessary to
		// modify the Name field of the returned header to provide the
		// full path name of the file.
		hdr.Name = relpath
		tarheader.Populate(hdr, info, inos)
		// If the file is a hard link to a file we've already seen, we
		// don't need the contents
		if hdr.Typeflag == tar.TypeLink {
			hdr.Size = 0
			r = nil
		}
		if err := aw.AddFile(relpath, hdr, r); err != nil {
			return err
		}

		return nil
	}
}

func BuildACI(imageID string, outDir string, targetDir string) (string, error) {
	targetACI := filepath.Join(outDir, imageID+".aci")

	mode := os.O_CREATE | os.O_WRONLY
	mode |= os.O_TRUNC

	fh, err := os.OpenFile(targetACI, mode, 0644)
	if err != nil {
		return "", fmt.Errorf("Unable to open target %s: %v", targetACI, err)
	}

	var r io.WriteCloser = fh
	tr := tar.NewWriter(r)

	defer func() {
		tr.Close()
		fh.Close()
	}()

	// TODO(jonboulle): stream the validation so we don't have to walk the rootfs twice
	if err := aci.ValidateLayout(targetDir); err != nil {
		return "", fmt.Errorf("Layout failed validation: %v", err)
	}
	mpath := filepath.Join(targetDir, aci.ManifestFile)
	b, err := ioutil.ReadFile(mpath)
	if err != nil {
		return "", fmt.Errorf("Unable to read Image Manifest: %v", err)

	}

	var im schema.ImageManifest
	if err := im.UnmarshalJSON(b); err != nil {
		return "", fmt.Errorf("Unable to load Image Manifest: %v", err)
	}
	iw := aci.NewImageWriter(im, tr)

	err = filepath.Walk(targetDir, buildWalker(targetDir, iw))
	if err != nil {
		return "", fmt.Errorf("Error walking rootfs: %v", err)
	}

	err = iw.Close()
	if err != nil {
		return "", fmt.Errorf("Unable to close image %s: %v", targetACI, err)
	}

	return targetACI, nil
}

func ImportLayer(layerID string, repoData *RepoData, dockerURL *DockerURL, dataStore *cas.Store, parentImageID string) (string, error) {
	tmpDir, err := ioutil.TempDir("", "docker2aci-")
	if err != nil {
		return "", fmt.Errorf("Error creating dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	layerDest := tmpDir + "/layer"
	layerRootfs := layerDest + "/rootfs"
	err = os.MkdirAll(layerRootfs, 0700)
	if err != nil {
		return "", fmt.Errorf("Error creating dir: %s", layerRootfs)
	}

	jsonString, size, err := GetRemoteImageJSON(layerID, repoData.Endpoints[0], repoData.Tokens)
	if err != nil {
		return "", fmt.Errorf("Error getting image json: %v", err)
	}

	layerData := DockerImageData{}
	if err := json.Unmarshal(jsonString, &layerData); err != nil {
		return "", fmt.Errorf("Error unmarshaling layer data: %v", err)
	}

	layer, err := GetRemoteLayer(layerID, repoData.Endpoints[0], repoData.Tokens, int64(size))
	if err != nil {
		return "", fmt.Errorf("Error getting the remote layer: %v", err)
	}
	defer layer.Close()

	err = archive.Untar(layer, layerRootfs, &archive.TarOptions{NoLchown: true})
	if err != nil {
		return "", fmt.Errorf("Error untaring image: %v", err)
	}

	manifest, err := GenerateManifest(layerData, dockerURL, parentImageID)
	if err != nil {
		return "", fmt.Errorf("Error generating the manifest: %v", err)
	}

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}

	f, err := os.Create(filepath.Join(layerDest, aci.ManifestFile))
	if err != nil {
		return "", fmt.Errorf("Error creating manifest file")
	}
	defer f.Close()

	f.Write(manifestBytes)
	f.Sync()

	aciPath, err := BuildACI(layerID, tmpDir, layerDest)
	if err != nil {
		return "", fmt.Errorf("Error building ACI: %v", err)
	}

	var aciFile *os.File
	if aciFile, err = os.Open(aciPath); err != nil {
		return "", fmt.Errorf("Error opening target aci file")
	}

	aciReader := bufio.NewReader(aciFile)
	parentImageID, err = dataStore.WriteACI(aciReader)
	if err != nil {
		return "", fmt.Errorf("Error writing ACI: %v", err)
	}
	aciFile.Close()

	return parentImageID, nil
}

func runDocker2ACI(arg string) error {
	dockerURL := parseArgument(arg)

	repoData, err := GetRepoData(dockerURL.IndexURL, dockerURL.ImageName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting image data: %v\n", err)
		return err
	}

	// TODO(iaguis) check more endpoints
	appImageID, err := GetImageIDFromTag(repoData.Endpoints[0], dockerURL.ImageName, dockerURL.Tag, repoData.Tokens)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting ImageID from tag %s: %v\n", dockerURL.Tag, err)
		return err
	}

	ancestry, err := GetAncestry(appImageID, repoData.Endpoints[0], repoData.Tokens)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting ancestry: %v\n", err)
		return err
	}

	ds := cas.NewStore(rocketDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open store: %v\n", err)
		return err
	}

	var rocketAppImageID string
	parentImageID := ""

	// From base image
	for i := len(ancestry) - 1; i >= 0; i-- {
		layerID := ancestry[i]

		rocketLayerID, err := ImportLayer(layerID, repoData, dockerURL, ds, parentImageID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error importing image: %v\n", err)
			return err
		}

		if layerID == appImageID {
			rocketAppImageID = rocketLayerID
		}
		parentImageID = rocketLayerID
	}

	fmt.Println(rocketAppImageID)
	return nil
}

func main() {
	flag.Parse()
	args := flag.Args()

	if len(args) != 1 {
		fmt.Println("Usage: docker2aci [REGISTRYURL/]IMAGE_NAME[:TAG]")
		return
	}

	if err := runDocker2ACI(args[0]); err != nil {
		os.Exit(1)
	}
}
