package feeds

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxFeedBytes = 8 << 20

// New feed clients retain the supplied transport but bound request time and
// decline redirects so credentials cannot cross to a different origin.
func feedClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	if copy.Timeout <= 0 || copy.Timeout > 15*time.Second {
		copy.Timeout = 15 * time.Second
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copy
}

func responseBody(client *http.Client, request *http.Request, source string) ([]byte, error) {
	response, err := client.Do(request)
	if err != nil {
		if contextError := request.Context().Err(); contextError != nil {
			return nil, contextError
		}
		return nil, fmt.Errorf("%w: %s request failed", ErrUnavailable, source)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		class := ErrUnavailable
		switch response.StatusCode {
		case http.StatusBadRequest:
			class = ErrInvalidQuery
		case http.StatusNotFound:
			class = ErrNotFound
		}
		return nil, fmt.Errorf("%w: %s returned HTTP %d", class, source, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		if contextError := request.Context().Err(); contextError != nil {
			return nil, contextError
		}
		return nil, fmt.Errorf("%w: %s response interrupted", ErrUnavailable, source)
	}
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("%w: %s response exceeds %d bytes", ErrUnavailable, source, maxFeedBytes)
	}
	return body, nil
}
