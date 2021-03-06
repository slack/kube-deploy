package fi

import (
	"fmt"
	"github.com/golang/glog"
	"io"
	"k8s.io/kube-deploy/upup/pkg/fi/hashing"
	"k8s.io/kube-deploy/upup/pkg/fi/utils"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

type asset struct {
	Key       string
	AssetPath string
	resource  Resource
	source    *Source
}

type Source struct {
	Parent             *Source
	URL                string
	Hash               *hashing.Hash
	ExtractFromArchive string
}

// Builds a unique key for this source
func (s *Source) Key() string {
	var k string
	if s.Parent != nil {
		k = s.Parent.Key() + "/"
	}
	if s.URL != "" {
		k += s.URL
	} else if s.ExtractFromArchive != "" {
		k += s.ExtractFromArchive
	} else {
		glog.Fatalf("expected either URL or ExtractFromArchive to be set")
	}
	return k
}

func (s *Source) String() string {
	return "Source[" + s.Key() + "]"
}

type HasSource interface {
	GetSource() *Source
}

// assetResource implements Resource, but also implements HasFetchInstructions
type assetResource struct {
	asset *asset
}

var _ Resource = &assetResource{}
var _ HasSource = &assetResource{}

func (r *assetResource) Open() (io.ReadSeeker, error) {
	return r.asset.resource.Open()
}

func (r *assetResource) GetSource() *Source {
	return r.asset.source
}

type AssetStore struct {
	assetDir string
	assets   []*asset
}

func NewAssetStore(assetDir string) *AssetStore {
	a := &AssetStore{
		assetDir: assetDir,
	}
	return a
}
func (a *AssetStore) Find(key string, assetPath string) (Resource, error) {
	var matches []*asset
	for _, asset := range a.assets {
		if asset.Key != key {
			continue
		}

		if assetPath != "" {
			if !strings.HasSuffix(asset.AssetPath, assetPath) {
				continue
			}
		}

		matches = append(matches, asset)
	}

	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) == 1 {
		glog.Infof("Resolved asset %s:%s to %s", key, assetPath, matches[0].AssetPath)
		return &assetResource{asset: matches[0]}, nil
	}

	glog.Infof("Matching assets:")
	for _, match := range matches {
		glog.Infof("    %s %s", match.Key, match.AssetPath)
	}
	return nil, fmt.Errorf("found multiple matching assets for key: %q", key)
}

func hashFromHttpHeader(url string) (*hashing.Hash, error) {
	glog.Infof("Doing HTTP HEAD on %q", url)
	response, err := http.Head(url)
	if err != nil {
		return nil, fmt.Errorf("error doing HEAD on %q: %v", url, err)
	}
	defer response.Body.Close()

	etag := response.Header.Get("ETag")
	etag = strings.TrimSpace(etag)
	etag = strings.Trim(etag, "'\"")

	if etag != "" {
		if len(etag) == 32 {
			// Likely md5
			return hashing.HashAlgorithmMD5.FromString(etag)
		}
	}

	return nil, fmt.Errorf("unable to determine hash from HTTP HEAD: %q", url)
}

func (a *AssetStore) Add(id string) error {
	if strings.HasSuffix(id, "http://") || strings.HasPrefix(id, "https://") {
		return a.addURL(id, nil)
	}
	// TODO: local files!
	return fmt.Errorf("unknown asset format: %q", id)
}

func (a *AssetStore) addURL(url string, hash *hashing.Hash) error {
	var err error

	if hash == nil {
		hash, err = hashFromHttpHeader(url)
		if err != nil {
			return err
		}
	}

	localFile := path.Join(a.assetDir, hash.String()+"_"+utils.SanitizeString(url))
	_, err = DownloadURL(url, localFile, hash)
	if err != nil {
		return err
	}

	key := path.Base(url)
	assetPath := url
	r := NewFileResource(localFile)

	source := &Source{URL: url, Hash: hash}

	asset := &asset{
		Key:       key,
		AssetPath: assetPath,
		resource:  r,
		source:    source,
	}
	glog.V(2).Infof("added asset %q for %q", asset.Key, asset.resource)
	a.assets = append(a.assets, asset)

	if strings.HasSuffix(assetPath, ".tar.gz") {
		err = a.addArchive(source, localFile)
		if err != nil {
			return err
		}
	}

	return nil
}

//func (a *AssetStore) addFile(assetPath string, p string) error {
//	r := NewFileResource(p)
//	return a.addResource(assetPath, r)
//}

//func (a *AssetStore) addResource(assetPath string, r Resource) error {
//	hash, err := HashForResource(r, HashAlgorithmSHA256)
//	if err != nil {
//		return err
//	}
//
//	localFile := path.Join(a.assetDir, hash + "_" + utils.SanitizeString(assetPath))
//	hasHash, err := fileHasHash(localFile, hash)
//	if err != nil {
//		return err
//	}
//
//	if !hasHash {
//		err = WriteFile(localFile, r, 0644, 0755)
//		if err != nil {
//			return err
//		}
//	}
//
//	asset := &asset{
//		Key:       localFile,
//		AssetPath: assetPath,
//		resource:  r,
//	}
//	glog.V(2).Infof("added asset %q for %q", asset.Key, asset.resource)
//	a.assets = append(a.assets, asset)
//
//	if strings.HasSuffix(assetPath, ".tar.gz") {
//		err = a.addArchive(localFile)
//		if err != nil {
//			return err
//		}
//	}
//
//	return nil
//}

func (a *AssetStore) addArchive(archiveSource *Source, archiveFile string) error {
	extracted := path.Join(a.assetDir, "extracted/"+path.Base(archiveFile))

	// TODO: Use a temp file so this is atomic
	if _, err := os.Stat(extracted); os.IsNotExist(err) {
		err := os.MkdirAll(extracted, 0755)
		if err != nil {
			return fmt.Errorf("error creating directories %q: %v", path.Dir(extracted), err)
		}

		args := []string{"tar", "zxf", archiveFile, "-C", extracted}
		glog.Infof("running extract command %s", args)
		cmd := exec.Command(args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error expanding asset file %q %v: %s", archiveFile, err, string(output))
		}
	}

	localBase := extracted
	assetBase := ""

	walker := func(localPath string, info os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("error descending into path %q: %v", localPath, err)
		}

		if info.IsDir() {
			return nil
		}

		relativePath, err := filepath.Rel(localBase, localPath)
		if err != nil {
			return fmt.Errorf("error finding relative path for %q: %v", localPath, err)
		}

		assetPath := path.Join(assetBase, relativePath)
		key := info.Name()
		r := NewFileResource(localPath)

		asset := &asset{
			Key:       key,
			AssetPath: assetPath,
			resource:  r,
			source:    &Source{Parent: archiveSource, ExtractFromArchive: assetPath},
		}
		glog.V(2).Infof("added asset %q for %q", asset.Key, asset.resource)
		a.assets = append(a.assets, asset)

		return nil
	}

	err := filepath.Walk(localBase, walker)
	if err != nil {
		return fmt.Errorf("error adding expanded asset files in %q: %v", extracted, err)
	}
	return nil

}
