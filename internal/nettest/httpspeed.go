package nettest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Putanje na konzoli za HTTP speed test.
const (
	SpeedDownloadPath = "/agent/speed/download"
	SpeedUploadPath   = "/agent/speed/upload"
	AgentTokenHeader  = "X-Agent-Token"
)

// HTTPDownloadWorker preuzima podatke sa konzole dok ctx ne istekne.
func HTTPDownloadWorker(client *http.Client, baseURL, token string, seconds int) Worker {
	return func(ctx context.Context, counter *atomic.Int64) error {
		url := baseURL + SpeedDownloadPath + "?seconds=" + strconv.Itoa(seconds+2)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set(AgentTokenHeader, token)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("download: HTTP %d", resp.StatusCode)
		}
		_, err = io.Copy(CountingWriter{counter}, resp.Body)
		return err
	}
}

// HTTPUploadWorker šalje podatke konzoli dok ctx ne istekne. Zahtevi su
// kratki (~1s) da bi se izbeglo dugo baferovanje i da bi se greške brzo videle.
// Bajtovi se broje u trenutku kada ih HTTP klijent preuzme za slanje.
func HTTPUploadWorker(client *http.Client, baseURL, token string) Worker {
	return func(ctx context.Context, counter *atomic.Int64) error {
		for ctx.Err() == nil {
			chunkCtx, cancel := context.WithTimeout(ctx, time.Second)
			body := &PayloadReader{Ctx: chunkCtx, Counter: counter}
			// Sam zahtev ne vezujemo za ctx: kada test istekne, telo vraća EOF
			// i zahtev se uredno završava umesto da se prekine usred slanja.
			reqCtx, reqCancel := context.WithTimeout(context.Background(), 15*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+SpeedUploadPath, body)
			if err != nil {
				cancel()
				reqCancel()
				return err
			}
			req.Header.Set(AgentTokenHeader, token)
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := client.Do(req)
			cancel()
			if err != nil {
				reqCancel()
				return err
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			reqCancel()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("upload: HTTP %d", resp.StatusCode)
			}
		}
		return nil
	}
}
