package hostd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/sparse"
)

// filesBaseURL and filesExtentsURL build the peer URLs for the two
// file-receive endpoints, escaping dir and name.
func filesBaseURL(targetURL, dir, name string) string {
	return targetURL + "/files/base?dir=" + url.QueryEscape(dir) + "&name=" + url.QueryEscape(name)
}

func filesExtentsURL(targetURL, dir, name string) string {
	return targetURL + "/files/extents?dir=" + url.QueryEscape(dir) + "&name=" + url.QueryEscape(name)
}

// postFile streams the whole file at path to targetURL over
// s.peerClient, with Content-Length set so the receiver never has to
// buffer to discover the size.
func (s *Server) postFile(ctx context.Context, targetURL, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, f)
	if err != nil {
		return 0, fmt.Errorf("new request: %w", err)
	}
	req.ContentLength = info.Size()

	fwr, err := s.doTransfer(req)
	if err != nil {
		return 0, fmt.Errorf("post file %s to %s: %w", path, targetURL, err)
	}
	return fwr.BytesWritten, nil
}

// postExtents walks the allocated extents of the file at path and
// streams them to targetURL in the contract wire format, without
// buffering the whole file: extents are read directly off disk into
// the request body as it is sent.
func (s *Server) postExtents(ctx context.Context, targetURL, path string) (int64, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	extents, err := sparse.WalkExtents(f)
	if err != nil {
		return 0, 0, fmt.Errorf("walk extents %s: %w", path, err)
	}

	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(sparse.StreamExtents(pw, f, extents))
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, pr)
	if err != nil {
		return 0, 0, fmt.Errorf("new request: %w", err)
	}

	fwr, err := s.doTransfer(req)
	if err != nil {
		return 0, 0, fmt.Errorf("post extents %s to %s: %w", path, targetURL, err)
	}
	return fwr.BytesWritten, fwr.Extents, nil
}

// doTransfer sends req over the pre-warmed peer client and decodes a
// FileWriteResponse from a successful reply.
func (s *Server) doTransfer(req *http.Request) (api.FileWriteResponse, error) {
	resp, err := s.peerClient.Do(req)
	if err != nil {
		return api.FileWriteResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return api.FileWriteResponse{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	var fwr api.FileWriteResponse
	if err := json.NewDecoder(resp.Body).Decode(&fwr); err != nil {
		return api.FileWriteResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return fwr, nil
}

// remoteLoad calls a peer hostd's /vms/{id}/load.
func (s *Server) remoteLoad(ctx context.Context, targetURL string, req api.LoadRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal load request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.peerClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("post %s: %w", targetURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post %s: status %d: %s", targetURL, resp.StatusCode, string(errBody))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
