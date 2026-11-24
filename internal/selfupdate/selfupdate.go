// Package selfupdate replaces the running plugyard program with the latest
// release.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultBase is where plugyard's releases are published.
const DefaultBase = "https://github.com/Amraa1/plugyard"

// ChecksumsName is the release file that lists the SHA-256 of every archive.
const ChecksumsName = "checksums.txt"

// maxDownload bounds what is read from the network.
const maxDownload = 200 << 20

// ErrNoRelease is returned when nothing has been released yet.
var ErrNoRelease = errors.New("no release of plugyard has been published yet")

// Updater fetches releases.
type Updater struct {
	// Base is the address of the project, such as DefaultBase.
	Base   string
	Client *http.Client
	// OS and Arch name the build to fetch, in Go's terms.
	OS, Arch string
	// UserAgent is sent with every request.
	UserAgent string
}

// AssetName returns the name of the release archive for a system.
func AssetName(goos, goarch string) string {
	name := "plugyard_" + goos + "_" + goarch
	if goos == "windows" {
		return name + ".zip"
	}
	return name + ".tar.gz"
}

// binaryName is the name of the program inside an archive.
func binaryName(goos string) string {
	if goos == "windows" {
		return "plugyard.exe"
	}
	return "plugyard"
}

func (u *Updater) get(ctx context.Context, url string, follow bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if u.UserAgent != "" {
		req.Header.Set("User-Agent", u.UserAgent)
	}
	client := http.Client{}
	if u.Client != nil {
		client = *u.Client
	}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client.Do(req)
}

// Latest returns the version tag of the latest release, such as "v0.2.0".
func (u *Updater) Latest(ctx context.Context) (string, error) {
	// The page of the latest release redirects to the page of its tag. That
	// answers the question without the API and its rate limit.
	resp, err := u.get(ctx, u.Base+"/releases/latest", false)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", u.Base, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrNoRelease
	case resp.StatusCode < 300 || resp.StatusCode > 399:
		return "", fmt.Errorf("%s/releases/latest answered %s", u.Base, resp.Status)
	}
	location := resp.Header.Get("Location")
	_, tag, found := strings.Cut(location, "/releases/tag/")
	if !found || tag == "" {
		// With no release, GitHub redirects to the list of releases.
		return "", ErrNoRelease
	}
	return path.Base(tag), nil
}

func (u *Updater) download(ctx context.Context, tag, name string) ([]byte, error) {
	url := u.Base + "/releases/download/" + tag + "/" + name
	resp, err := u.get(ctx, url, true)
	if err != nil {
		return nil, fmt.Errorf("cannot download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cannot download %s: %s answered %s", name, url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("cannot download %s: %w", name, err)
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("%s is larger than %d MB, which cannot be right", name, maxDownload>>20)
	}
	return data, nil
}

// Download fetches the program of a release for the updater's system, and
// checks it against the release's checksums.
func (u *Updater) Download(ctx context.Context, tag string) ([]byte, error) {
	asset := AssetName(u.OS, u.Arch)
	sums, err := u.download(ctx, tag, ChecksumsName)
	if err != nil {
		return nil, err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			want = strings.ToLower(fields[0])
		}
	}
	if want == "" {
		return nil, fmt.Errorf("release %s has no build for %s/%s", tag, u.OS, u.Arch)
	}

	archive, err := u.download(ctx, tag, asset)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s does not match its checksum; nothing was changed", asset)
	}
	return extract(archive, asset, binaryName(u.OS))
}

// extract returns the file called name from a .zip or .tar.gz archive.
func extract(archive []byte, assetName, name string) ([]byte, error) {
	missing := fmt.Errorf("%s holds no %s", assetName, name)
	if strings.HasSuffix(assetName, ".zip") {
		r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("%s cannot be read: %w", assetName, err)
		}
		for _, f := range r.File {
			if path.Base(f.Name) != name {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, maxDownload))
		}
		return nil, missing
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read: %w", assetName, err)
	}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, missing
		}
		if err != nil {
			return nil, fmt.Errorf("%s cannot be read: %w", assetName, err)
		}
		if header.Typeflag == tar.TypeReg && path.Base(header.Name) == name {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// oldSuffix marks the program an update replaced. Windows cannot delete a
// program that is running, only rename it.
const oldSuffix = ".old"

// Replace puts a new program in place of the one at exePath. If it fails,
// the program that was there is still there.
func Replace(exePath string, program []byte) error {
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, filepath.Base(exePath)+".new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(program); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}

	// Move the running program aside, then move the new one in.
	old := exePath + oldSuffix
	_ = os.Remove(old)
	if err := os.Rename(exePath, old); err != nil {
		return err
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		_ = os.Rename(old, exePath)
		return err
	}
	// Where the old program can be deleted while it runs, it goes now;
	// elsewhere CleanOld removes it the next time plugyard starts.
	_ = os.Remove(old)
	return nil
}

// CleanOld removes what an earlier update left beside the program.
func CleanOld(exePath string) {
	_ = os.Remove(exePath + oldSuffix)
}

// Newer reports whether version latest is newer than version current. A
// current version that is not a release, such as "dev", is older than any
// release.
func Newer(current, latest string) bool {
	if current == latest {
		return false
	}
	have, havePre, ok := parse(current)
	want, wantPre, okLatest := parse(latest)
	if !okLatest {
		return false
	}
	if !ok {
		return true
	}
	for i := range have {
		if have[i] != want[i] {
			return want[i] > have[i]
		}
	}
	// With the same numbers, a release is newer than its pre-releases.
	switch {
	case havePre != "" && wantPre == "":
		return true
	case havePre == "" || wantPre == "":
		return false
	}
	return wantPre > havePre
}

// parse reads a version such as v1.2.3 or v1.2.3-beta.1.
func parse(version string) (numbers [3]int, pre string, ok bool) {
	text, found := strings.CutPrefix(version, "v")
	if !found {
		return numbers, "", false
	}
	text, _, _ = strings.Cut(text, "+")
	text, pre, _ = strings.Cut(text, "-")
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return numbers, "", false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return numbers, "", false
		}
		numbers[i] = n
	}
	return numbers, pre, true
}
