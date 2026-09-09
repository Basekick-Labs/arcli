// Package sample fetches published sample datasets so `arcli sample` can
// load real data into a user's own Arc.
//
// This is the one place in arcli that contacts a Basekick-controlled host
// rather than the Arc servers the user configured. Two properties matter:
//
//   - Requests carry the same identity headers as any other arcli request:
//     the User-Agent and the installation id (Arcli-Installation-Id). That
//     is what lets a sample download be correlated with the Arc instance it
//     was loaded into, which is the point of publishing the datasets. The
//     existing opt-outs apply unchanged: DO_NOT_TRACK=1 or
//     send_installation_id = false in the config file suppress the header
//     here exactly as they do everywhere else.
//   - Every file is verified against the SHA-256 in the manifest before it
//     is handed to the importer, so a truncated or tampered download fails
//     loudly instead of importing quietly.
package sample

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// BaseURL is where published datasets live. Hardcoded rather than
// configurable: an override would be a way to point `sample load` at an
// arbitrary host, and anyone who wants that already has
// `arcli import parquet`.
const BaseURL = "https://samples.basekick.net"

// ManifestSchemaVersion is the only manifest layout this build understands.
// A newer server-side manifest is a clean error telling the user to upgrade,
// not a partial parse.
const ManifestSchemaVersion = 1

// Limits on what we will read from the network. The manifest is small and
// the parts are tens of MiB; these ceilings stop a misbehaving or hostile
// endpoint from filling the disk. Each part is additionally bounded by its
// own manifest-declared size (see fetchPart).
const (
	maxManifestBytes = 8 << 20   // 8 MiB
	maxPartBytes     = 512 << 20 // 512 MiB
)

// Manifest is the published index of datasets.
type Manifest struct {
	SchemaVersion int                `json:"schema_version"`
	Datasets      map[string]Dataset `json:"datasets"`
}

// Dataset describes one logical dataset and the months published for it.
type Dataset struct {
	Title             string           `json:"title"`
	Description       string           `json:"description"`
	SourceURL         string           `json:"source_url"`
	License           string           `json:"license"`
	Measurement       string           `json:"measurement"`
	SuggestedDatabase string           `json:"suggested_database"`
	Months            map[string]Month `json:"months"`
}

// Month is one published month of a dataset.
type Month struct {
	Rows    int64  `json:"rows"`
	Bytes   int64  `json:"bytes"`
	Files   int    `json:"files"`
	TimeMin string `json:"time_min"`
	TimeMax string `json:"time_max"`
	Parts   []Part `json:"parts"`
}

// Part is a single downloadable file.
type Part struct {
	Key    string `json:"key"`
	Bytes  int64  `json:"bytes"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

// URL is where this part is published. Used for display and for the
// -o json output that lets someone fetch the files without arcli;
// downloads go through Client.partURL so tests can retarget the host.
func (p Part) URL() string { return BaseURL + "/" + p.Key }

// Name is the part's bare filename, used as its name on disk.
func (p Part) Name() string { return path.Base(p.Key) }

// HeaderInstallationID carries the CLI installation id, the same header
// name the Arc client sends. Duplicated rather than imported so this
// package does not depend on internal/client.
const HeaderInstallationID = "Arcli-Installation-Id"

// Client fetches manifests and parts.
type Client struct {
	httpClient     *http.Client
	userAgent      string
	installationID string
	baseURL        string
}

// NewClient returns a Client with timeouts suited to downloading tens of
// MiB over a slow link.
//
// userAgent must be the same string the rest of the CLI sends; the gate in
// front of the bucket requires the arcli shape. installationID is the id
// from the config file, already resolved through the opt-outs
// (DO_NOT_TRACK, send_installation_id): pass "" and no id header is sent.
func NewClient(userAgent, installationID string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
			// Never follow a redirect off the sample host. A redirect
			// chain is how an unexpected third party would end up
			// receiving these requests.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("unexpected redirect to %s", req.URL.Host)
			},
		},
		userAgent:      userAgent,
		installationID: installationID,
		baseURL:        BaseURL,
	}
}

// get issues a GET carrying the same identity headers as any other arcli
// request: User-Agent, and the installation id unless it was opted out.
// No token and no Arc endpoint are ever sent here.
func (c *Client) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if c.installationID != "" {
		req.Header.Set(HeaderInstallationID, c.installationID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, statusError(url, resp.StatusCode)
	}
	return resp, nil
}

// partURL is where a part is actually fetched from: the client's base
// host plus the manifest key. Production always resolves to Part.URL();
// tests point baseURL at a local server.
func (c *Client) partURL(p Part) string {
	return strings.TrimSuffix(c.baseURL, "/") + "/" + p.Key
}

// statusError turns an HTTP status into something a user can act on. The
// 403 case is worth special handling: it means the gate rejected our
// User-Agent, which is a build problem rather than anything the user did.
func statusError(url string, code int) error {
	switch code {
	case http.StatusForbidden:
		return fmt.Errorf("fetching %s: refused by the sample host (HTTP 403); this build's User-Agent was not accepted, please report it", url)
	case http.StatusNotFound:
		return fmt.Errorf("fetching %s: not found (HTTP 404); the manifest may be newer than this arcli build", url)
	default:
		return fmt.Errorf("fetching %s: HTTP %d", url, code)
	}
}

// Manifest fetches and validates the dataset index.
func (c *Client) Manifest(ctx context.Context) (*Manifest, error) {
	resp, err := c.get(ctx, c.baseURL+"/manifest.json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.SchemaVersion != ManifestSchemaVersion {
		return nil, fmt.Errorf(
			"manifest schema version %d is not supported by this arcli build (expected %d); upgrade arcli",
			m.SchemaVersion, ManifestSchemaVersion)
	}
	if len(m.Datasets) == 0 {
		return nil, errors.New("manifest lists no datasets")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

// validate rejects manifests this client cannot download safely. Each
// check exists because the failure it prevents is silent rather than
// loud: a part with no key writes to the directory itself, two parts
// sharing a basename overwrite each other on disk, and a negative size
// disables the download bound in fetchPart.
func (m *Manifest) validate() error {
	for id, d := range m.Datasets {
		for mk, mo := range d.Months {
			seen := make(map[string]string, len(mo.Parts))
			for _, p := range mo.Parts {
				where := fmt.Sprintf("%s/%s", id, mk)
				if strings.TrimSpace(p.Key) == "" {
					return fmt.Errorf("%s: a part has an empty key", where)
				}
				if p.Bytes < 0 {
					return fmt.Errorf("%s: part %s declares a negative size (%d)", where, p.Key, p.Bytes)
				}
				name := p.Name()
				if name == "" || name == "." || name == "/" {
					return fmt.Errorf("%s: part %q has no usable filename", where, p.Key)
				}
				if prev, dup := seen[name]; dup {
					return fmt.Errorf("%s: parts %q and %q share the filename %q", where, prev, p.Key, name)
				}
				seen[name] = p.Key
			}
		}
	}
	return nil
}

// DatasetIDs returns the dataset names in stable order.
func (m *Manifest) DatasetIDs() []string {
	ids := make([]string, 0, len(m.Datasets))
	for id := range m.Datasets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Dataset looks up one dataset, erroring with the available ids so a typo
// is self-correcting.
func (m *Manifest) Dataset(id string) (Dataset, error) {
	d, ok := m.Datasets[id]
	if !ok {
		return Dataset{}, fmt.Errorf("unknown dataset %q (available: %s)", id, strings.Join(m.DatasetIDs(), ", "))
	}
	return d, nil
}

// MonthKeys returns a dataset's months in ascending order. Keys are
// YYYY-MM, so a lexical sort is chronological.
func (d Dataset) MonthKeys() []string {
	keys := make([]string, 0, len(d.Months))
	for k := range d.Months {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Month resolves a month key, defaulting to the newest when key is empty.
func (d Dataset) Month(key string) (string, Month, error) {
	keys := d.MonthKeys()
	if len(keys) == 0 {
		return "", Month{}, errors.New("dataset has no published months")
	}
	if key == "" {
		key = keys[len(keys)-1]
	}
	mo, ok := d.Months[key]
	if !ok {
		return "", Month{}, fmt.Errorf("unknown month %q (available: %s)", key, strings.Join(keys, ", "))
	}
	if len(mo.Parts) == 0 {
		return "", Month{}, fmt.Errorf("month %q lists no files", key)
	}
	return key, mo, nil
}

// DownloadDir is where parts for a dataset+month are cached.
//
// Deliberately deterministic rather than a fresh MkdirTemp: a re-run after
// an interrupted download reuses verified files instead of orphaning a
// random directory, and ctrl-C skips deferred cleanup, so a random dir
// would leak tens of MiB per interrupted run.
func DownloadDir(root, dataset, month string) string {
	if root == "" {
		root = filepath.Join(os.TempDir(), "arcli-sample")
	}
	return filepath.Join(root, dataset, month)
}

// Progress reports per-file download outcomes.
type Progress func(index, total int, p Part, cached bool)

// Fetch downloads every part of a month into dir, verifying each against
// its manifest checksum, and returns the local paths in manifest order.
//
// A file already present with a matching checksum is reused. A file
// present with the wrong checksum is deleted and downloaded once more,
// since the common cause is an interrupted earlier run rather than
// corruption at the source; a second mismatch is a hard error.
func (c *Client) Fetch(ctx context.Context, dir string, parts []Part, concurrency int, progress Progress) ([]string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating download directory: %w", err)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	paths := make([]string, len(parts))
	for i, p := range parts {
		paths[i] = filepath.Join(dir, p.Name())
	}

	type result struct {
		index  int
		cached bool
		err    error
	}
	// Cancel the remaining downloads as soon as one fails. Without this a
	// failure that is going to repeat for every part -- a 403 because the
	// gate did not recognise this build, say -- would still pull the whole
	// dataset before reporting it.
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	sem := make(chan struct{}, concurrency)
	results := make(chan result, len(parts))

	// Wait for every goroutine before returning, so no straggler is still
	// writing into dir after the caller has moved on (the caller may
	// delete dir on the import path).
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		go func(i int, p Part) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results <- result{index: i, err: ctx.Err()}
				return
			}
			if ctx.Err() != nil {
				results <- result{index: i, err: ctx.Err()}
				return
			}
			cached, err := c.ensurePart(ctx, paths[i], p)
			results <- result{index: i, cached: cached, err: err}
		}(i, p)
	}

	var firstErr error
	ok := 0
	for range parts {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				// Stop the rest; their ctx.Err() results are drained by
				// this same loop.
				cancelAll()
			}
			continue
		}
		// Count only successes, so "downloaded 12/12" can never be
		// printed for a run where a file failed.
		ok++
		if progress != nil {
			progress(ok, len(parts), parts[r.index], r.cached)
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return paths, nil
}

// ensurePart makes dst exist with the checksum p declares, reporting
// whether it was already there.
func (c *Client) ensurePart(ctx context.Context, dst string, p Part) (bool, error) {
	if ok, err := checksumMatches(dst, p.SHA256); err != nil {
		return false, err
	} else if ok {
		return true, nil
	}
	// Either absent, or present and wrong. Remove any partial file so the
	// download starts from a known state.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("removing stale %s: %w", filepath.Base(dst), err)
	}
	if err := c.fetchPart(ctx, dst, p); err != nil {
		return false, err
	}
	ok, err := checksumMatches(dst, p.SHA256)
	if err != nil {
		return false, err
	}
	if !ok {
		os.Remove(dst)
		return false, fmt.Errorf("checksum mismatch for %s after download; the file was not imported", p.Name())
	}
	return false, nil
}

// fetchPart downloads one part to dst, streaming to a temp file in the
// same directory and renaming on success so an interrupted download never
// leaves a file that looks complete.
func (c *Client) fetchPart(ctx context.Context, dst string, p Part) error {
	resp, err := c.get(ctx, c.partURL(p))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed
	}()

	// Bound the read by what the manifest says the file is, with a little
	// slack, so a wrong-sized response fails here rather than filling the
	// disk. maxPartBytes is the absolute ceiling.
	limit := p.Bytes + 1
	if limit <= 0 || limit > maxPartBytes {
		limit = maxPartBytes
	}
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("downloading %s: %w", p.Name(), err)
	}
	if p.Bytes > 0 && written != p.Bytes {
		return fmt.Errorf("downloading %s: expected %d bytes, got %d", p.Name(), p.Bytes, written)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", p.Name(), err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("finalizing %s: %w", p.Name(), err)
	}
	return nil
}

// checksumMatches reports whether path exists and hashes to want.
// A missing file is (false, nil): the caller downloads it.
func checksumMatches(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("opening %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want), nil
}
