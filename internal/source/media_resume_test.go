package source

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestMediaReadsAllPartialResponses(t *testing.T) {
	const data = "0123456789ab"
	for _, unknown := range []bool{false, true} {
		total := strconv.Itoa(len(data))
		if unknown {
			total = "*"
		}
		t.Run("total="+total, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := readRangeOffset(t, w, r)
				if start < 0 {
					return
				}
				if start >= len(data) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return
				}
				end := min(start+2, len(data))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%s", start, end-1, total))
				w.WriteHeader(http.StatusPartialContent)
				fmt.Fprint(w, data[start:end])
			}))
			defer origin.Close()
			service := &Service{HTTP: origin.Client()}
			body, err := service.openSeekable(t.Context(), origin.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			got, err := io.ReadAll(body)
			if string(got) != data || err != nil {
				t.Fatalf("read %q, err = %v, want %q", got, err, data)
			}
			if _, err := body.(io.Seeker).Seek(-3, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			got, err = io.ReadAll(body)
			if string(got) != data[len(data)-3:] || err != nil {
				t.Fatalf("read after seek: %q, err = %v", got, err)
			}
		})
	}
}

func TestMediaRecoversFromTruncatedPartialResponse(t *testing.T) {
	const data = "0123456789"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := readRangeOffset(t, w, r)
		switch start {
		case 0:
			// Deliver less than the Content-Range claims, over chunked HTTP
			// so the transport itself cannot tell the response is incomplete.
			w.Header().Set("Content-Range", "bytes 0-9/10")
			w.WriteHeader(http.StatusPartialContent)
			w.(http.Flusher).Flush()
			fmt.Fprint(w, data[:4])
		case 4:
			// A server can ignore Range; the repeated prefix must be discarded.
			fmt.Fprint(w, data)
		default:
			t.Errorf("unexpected resume offset %d", start)
			http.Error(w, "bad range", http.StatusBadRequest)
		}
	}))
	defer origin.Close()
	service := &Service{HTTP: origin.Client()}
	body, err := service.openSeekable(t.Context(), origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if string(got) != data || err != nil {
		t.Fatalf("read %q, err = %v, want %q", got, err, data)
	}
}

func TestMediaRejectsRangeStartingAtWrongOffset(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "0123")
	}))
	defer origin.Close()
	service := &Service{HTTP: origin.Client()}
	body, err := service.openSeekable(t.Context(), origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if string(got) != "0123" || err == nil || !strings.Contains(err.Error(), "invalid Content-Range") {
		t.Fatalf("read %q, err = %v, want first segment followed by a range error", got, err)
	}
}

func readRangeOffset(t *testing.T, w http.ResponseWriter, r *http.Request) int {
	t.Helper()
	var start int
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start); err != nil || start < 0 {
		t.Errorf("invalid Range %q", r.Header.Get("Range"))
		http.Error(w, "bad range", http.StatusBadRequest)
		return -1
	}
	return start
}
