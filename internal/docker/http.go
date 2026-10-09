package docker

import (
	"context"
	"net/http"
	"time"
)

var httpClient = &http.Client{Timeout: 2 * time.Second}

func newGET(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}
