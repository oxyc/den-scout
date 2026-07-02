package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// Debrid stores (ported from src/stores/*). Two ops: CacheCheck (which hashes are cached?) and Resolve
// (infohash → playable https link). Scout resolves server-side; the token never leaves the server.

// ResolveTarget: an infohash + either the exact file, or the series episode to pick from a pack.
type ResolveTarget struct {
	InfoHash string
	FileIdx  *int
	Season   *int
	Episode  *int
}

// Store is a debrid backend. CacheCheck MUST map missing/failed hashes to false, never error.
type Store interface {
	Service() DebridService
	CacheCheck(ctx context.Context, hashes []string) map[string]bool
	Resolve(ctx context.Context, t ResolveTarget) (string, error)
}

// DeadLinkError — nothing could deliver the file → the route answers 404 so the client falls through.
type DeadLinkError struct{ Reason string }

func (e *DeadLinkError) Error() string { return "dead_link: " + e.Reason }

func magnetFor(infoHash string) string { return "magnet:?xt=urn:btih:" + infoHash }

const cacheBatch = 100 // TorBox/Premiumize hashes per checkcached call

// --- TorBox ---

const torboxAPI = "https://api.torbox.app/v1/api"
const resolveCacheTTL = 6 * time.Hour

type torBoxStore struct {
	token  string
	client doer
	cache  Cache
	api    string
}

func (s *torBoxStore) Service() DebridService { return ServiceTorBox }

func (s *torBoxStore) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("accept", "application/json")
	return s.client.Do(req)
}

func (s *torBoxStore) CacheCheck(ctx context.Context, hashes []string) map[string]bool {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	cached := make([]bool, len(hashes)) // distinct-index writes → concurrency-safe without a lock
	g, gctx := errgroup.WithContext(ctx)
	for start := 0; start < len(hashes); start += cacheBatch {
		start := start
		end := start + cacheBatch
		if end > len(hashes) {
			end = len(hashes)
		}
		g.Go(func() error {
			batch := hashes[start:end]
			u := fmt.Sprintf("%s/torrents/checkcached?hash=%s&format=object&list_files=false", s.api, strings.Join(batch, ","))
			resp, err := s.get(gctx, u)
			if err != nil {
				return nil
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var body struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) != nil {
				return nil
			}
			for i, h := range batch {
				if _, ok := body.Data[h]; ok {
					cached[start+i] = true
				} else if _, ok := body.Data[strings.ToUpper(h)]; ok {
					cached[start+i] = true
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	for i, h := range hashes {
		if cached[i] {
			result[h] = true
		}
	}
	return result
}

type torboxResolveEntry struct {
	TorrentID int           `json:"torrentId"`
	Files     []TorrentFile `json:"files"`
}

func (s *torBoxStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	needFiles := t.FileIdx == nil && t.Season != nil && t.Episode != nil
	key := "torbox:resolve:" + t.InfoHash

	// Fast path: a warm entry from an earlier episode of the same pack. Skip it when episode-select is
	// needed but the cached file list is empty (audit #3 — a transient blip would otherwise mis-serve).
	if s.cache != nil {
		if raw, ok := s.cache.Get(key); ok {
			var e torboxResolveEntry
			if json.Unmarshal([]byte(raw), &e) == nil && (!needFiles || len(e.Files) > 0) {
				if link, err := s.requestDownload(ctx, e.TorrentID, selectFileID(e.Files, t)); err == nil {
					return link, nil
				}
			}
		}
	}

	torrentID, err := s.addMagnet(ctx, t.InfoHash)
	if err != nil {
		return "", err
	}
	var files []TorrentFile
	if needFiles {
		files = s.listFiles(ctx, torrentID)
	}
	// audit #3: don't cache an empty file list when we needed one (avoids poisoning the pack for 6h).
	if s.cache != nil && (!needFiles || len(files) > 0) {
		if b, e := json.Marshal(torboxResolveEntry{TorrentID: torrentID, Files: files}); e == nil {
			s.cache.Put(key, string(b), resolveCacheTTL)
		}
	}
	return s.requestDownload(ctx, torrentID, selectFileID(files, t))
}

func (s *torBoxStore) addMagnet(ctx context.Context, infoHash string) (int, error) {
	form := url.Values{"magnet": {magnetFor(infoHash)}, "seed": {"3"}, "allow_zip": {"false"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/torrents/createtorrent", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, &DeadLinkError{fmt.Sprintf("torbox createtorrent http %d", resp.StatusCode)}
	}
	var body struct {
		Data *struct {
			TorrentID *int `json:"torrent_id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Data == nil || body.Data.TorrentID == nil {
		return 0, &DeadLinkError{"torbox no torrent_id"}
	}
	return *body.Data.TorrentID, nil
}

func (s *torBoxStore) listFiles(ctx context.Context, torrentID int) []TorrentFile {
	resp, err := s.get(ctx, fmt.Sprintf("%s/torrents/mylist?id=%d&bypass_cache=true", s.api, torrentID))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return nil
	}
	type tbFile struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		Size      *int   `json:"size"`
	}
	type entry struct {
		Files []tbFile `json:"files"`
	}
	var e entry
	if json.Unmarshal(body.Data, &e) != nil {
		var arr []entry
		if json.Unmarshal(body.Data, &arr) != nil || len(arr) == 0 {
			return nil
		}
		e = arr[0]
	}
	out := make([]TorrentFile, 0, len(e.Files))
	for _, f := range e.Files {
		name := f.Name
		if name == "" {
			name = f.ShortName
		}
		out = append(out, TorrentFile{Index: f.ID, Name: name, SizeBytes: f.Size})
	}
	return out
}

func (s *torBoxStore) requestDownload(ctx context.Context, torrentID int, fileID *int) (string, error) {
	q := url.Values{"token": {s.token}, "torrent_id": {fmt.Sprintf("%d", torrentID)}}
	if fileID != nil {
		q.Set("file_id", fmt.Sprintf("%d", *fileID))
	}
	resp, err := s.get(ctx, s.api+"/torrents/requestdl?"+q.Encode())
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &DeadLinkError{fmt.Sprintf("torbox requestdl http %d", resp.StatusCode)}
	}
	var body struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || !body.Success || body.Data == "" {
		return "", &DeadLinkError{"torbox no link"}
	}
	return body.Data, nil
}

// selectFileID: explicit fileIdx, else the episode pick from an already-loaded file list.
func selectFileID(files []TorrentFile, t ResolveTarget) *int {
	if t.FileIdx != nil {
		return t.FileIdx
	}
	if t.Season == nil || t.Episode == nil || len(files) == 0 {
		return nil
	}
	return pickEpisodeFile(files, *t.Season, *t.Episode)
}

// --- Real-Debrid ---

const realDebridAPI = "https://api.real-debrid.com/rest/1.0"

type realDebridStore struct {
	token  string
	client doer
	api    string
}

func (s *realDebridStore) Service() DebridService { return ServiceRealDebrid }

// CacheCheck: RD has no usable cache API → all-false (TorBox/Premiumize provide cache truth).
func (s *realDebridStore) CacheCheck(_ context.Context, hashes []string) map[string]bool {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	return result
}

func (s *realDebridStore) post(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	return s.client.Do(req)
}

type rdInfo struct {
	Files []struct {
		ID    int    `json:"id"`
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	} `json:"files"`
	Links []string `json:"links"`
}

func (s *realDebridStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	addResp, err := s.post(ctx, "/torrents/addMagnet", url.Values{"magnet": {magnetFor(t.InfoHash)}})
	if err != nil {
		return "", err
	}
	var added struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(addResp.Body)
	_ = dec.Decode(&added)
	_ = addResp.Body.Close()
	if addResp.StatusCode < 200 || addResp.StatusCode >= 300 || added.ID == "" {
		return "", &DeadLinkError{"realdebrid no torrent id"}
	}

	info, err := s.info(ctx, added.ID)
	if err != nil {
		return "", err
	}
	files := make([]TorrentFile, len(info.Files))
	for i, f := range info.Files {
		size := f.Bytes
		files[i] = TorrentFile{Index: f.ID, Name: f.Path, SizeBytes: &size}
	}
	fileID := s.pickFileID(files, t)
	if fileID == nil {
		return "", &DeadLinkError{"realdebrid no file"}
	}
	// RD rejects anti-piracy-matched filenames — fail fast so the pool falls through to another store.
	for _, f := range files {
		if f.Index == *fileID && realDebridBlocked(f.Name) {
			return "", &DeadLinkError{"realdebrid blocked filename"}
		}
	}

	sel, err := s.post(ctx, "/torrents/selectFiles/"+added.ID, url.Values{"files": {fmt.Sprintf("%d", *fileID)}})
	if err != nil {
		return "", err
	}
	_ = sel.Body.Close()
	if sel.StatusCode < 200 || sel.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("realdebrid selectFiles http %d", sel.StatusCode)}
	}
	ready, err := s.info(ctx, added.ID)
	if err != nil {
		return "", err
	}
	if len(ready.Links) == 0 {
		return "", &DeadLinkError{"realdebrid not ready"}
	}
	return s.unrestrict(ctx, ready.Links[0])
}

func (s *realDebridStore) info(ctx context.Context, id string) (*rdInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.api+"/torrents/info/"+id, nil)
	req.Header.Set("authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &DeadLinkError{fmt.Sprintf("realdebrid info http %d", resp.StatusCode)}
	}
	var info rdInfo
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return nil, &DeadLinkError{"realdebrid bad info"}
	}
	return &info, nil
}

func (s *realDebridStore) unrestrict(ctx context.Context, link string) (string, error) {
	resp, err := s.post(ctx, "/unrestrict/link", url.Values{"link": {link}})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("realdebrid unrestrict http %d", resp.StatusCode)}
	}
	var body struct {
		Download string `json:"download"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Download == "" {
		return "", &DeadLinkError{"realdebrid no download"}
	}
	return body.Download, nil
}

func (s *realDebridStore) pickFileID(files []TorrentFile, t ResolveTarget) *int {
	if len(files) == 0 {
		return nil
	}
	if t.FileIdx != nil {
		if *t.FileIdx >= 0 && *t.FileIdx < len(files) {
			return &files[*t.FileIdx].Index
		}
		return &files[0].Index
	}
	if t.Season != nil && t.Episode != nil {
		return pickEpisodeFile(files, *t.Season, *t.Episode)
	}
	idx := largest(files).Index
	return &idx
}

// --- Premiumize ---

const premiumizeAPI = "https://www.premiumize.me/api"

type premiumizeStore struct {
	token  string
	client doer
	api    string
}

func (s *premiumizeStore) Service() DebridService { return ServicePremiumize }

func (s *premiumizeStore) CacheCheck(ctx context.Context, hashes []string) map[string]bool {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	cached := make([]bool, len(hashes))
	g, gctx := errgroup.WithContext(ctx)
	for start := 0; start < len(hashes); start += cacheBatch {
		start := start
		end := start + cacheBatch
		if end > len(hashes) {
			end = len(hashes)
		}
		g.Go(func() error {
			batch := hashes[start:end]
			q := url.Values{"apikey": {s.token}}
			for _, h := range batch {
				q.Add("items[]", h)
			}
			req, _ := http.NewRequestWithContext(gctx, http.MethodGet, s.api+"/cache/check?"+q.Encode(), nil)
			resp, err := s.client.Do(req)
			if err != nil {
				return nil
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var body struct {
				Status   string `json:"status"`
				Response []bool `json:"response"`
			}
			if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Status != "success" {
				return nil
			}
			for i := range batch {
				if i < len(body.Response) && body.Response[i] {
					cached[start+i] = true
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	for i, h := range hashes {
		if cached[i] {
			result[h] = true
		}
	}
	return result
}

func (s *premiumizeStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	form := url.Values{"apikey": {s.token}, "src": {magnetFor(t.InfoHash)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/transfer/directdl", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("premiumize directdl http %d", resp.StatusCode)}
	}
	var body struct {
		Status  string `json:"status"`
		Content []struct {
			Path string `json:"path"`
			Link string `json:"link"`
			Size *int   `json:"size"`
		} `json:"content"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Status != "success" || len(body.Content) == 0 {
		return "", &DeadLinkError{"premiumize no content"}
	}
	files := make([]TorrentFile, len(body.Content))
	for i, c := range body.Content {
		files[i] = TorrentFile{Index: i, Name: c.Path, SizeBytes: c.Size}
	}
	idx := s.pickIndex(files, t)
	if idx == nil || *idx < 0 || *idx >= len(body.Content) || body.Content[*idx].Link == "" {
		return "", &DeadLinkError{"premiumize no link"}
	}
	return body.Content[*idx].Link, nil
}

func (s *premiumizeStore) pickIndex(files []TorrentFile, t ResolveTarget) *int {
	if len(files) == 0 {
		return nil
	}
	if t.FileIdx != nil && *t.FileIdx >= 0 && *t.FileIdx < len(files) {
		return t.FileIdx
	}
	if t.Season != nil && t.Episode != nil {
		return pickEpisodeFile(files, *t.Season, *t.Episode)
	}
	idx := largest(files).Index
	return &idx
}

// --- pool ---

// StorePool builds one store per account in service-priority order (TorBox first).
type StorePool struct{ stores []Store }

func buildStores(config *Config, client doer, cache Cache) []Store {
	byService := make(map[DebridService]string)
	for _, d := range config.Debrid {
		byService[d.Service] = d.Token
	}
	var stores []Store
	for _, svc := range debridServices {
		token, ok := byService[svc]
		if !ok {
			continue
		}
		switch svc {
		case ServiceTorBox:
			stores = append(stores, &torBoxStore{token: token, client: client, cache: cache, api: torboxAPI})
		case ServiceRealDebrid:
			stores = append(stores, &realDebridStore{token: token, client: client, api: realDebridAPI})
		case ServicePremiumize:
			stores = append(stores, &premiumizeStore{token: token, client: client, api: premiumizeAPI})
		}
	}
	return stores
}

func (p *StorePool) CacheCheck(ctx context.Context, hashes []string) map[string]bool {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	if len(hashes) == 0 {
		return result
	}
	// Independent per store; run concurrently and union. Each store's CacheCheck is failure-tolerant
	// (returns a map, never errors), so one slow/failing store can't 500 the request (audit #5).
	maps := make([]map[string]bool, len(p.stores))
	var g errgroup.Group
	for i, st := range p.stores {
		i, st := i, st
		g.Go(func() error { maps[i] = st.CacheCheck(ctx, hashes); return nil })
	}
	_ = g.Wait()
	for _, m := range maps {
		for h, c := range m {
			if c {
				result[h] = true
			}
		}
	}
	return result
}

func (p *StorePool) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	for _, st := range p.stores {
		if link, err := st.Resolve(ctx, t); err == nil {
			return link, nil
		}
	}
	return "", &DeadLinkError{"no store could resolve"}
}

// hasCacheTruth reports whether any configured store has a real cache API (TorBox/Premiumize). When
// false (RD-only), the handler skips the cached-only filter so the list isn't always empty (audit #4).
func hasCacheTruth(config *Config) bool {
	for _, d := range config.Debrid {
		if d.Service == ServiceTorBox || d.Service == ServicePremiumize {
			return true
		}
	}
	return false
}
