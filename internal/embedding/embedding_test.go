package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFakeAndCosine(t *testing.T) {
	f := &Fake{Dims: 32, Synonyms: map[string]string{"collection": "pickup"}}
	v, err := f.Embed(context.Background(), []string{"parcel pickup slot", "parcel collection slot", "invoice margin"})
	if err != nil {
		t.Fatal(err)
	}
	if c := Cosine(v[0], v[1]); c < 0.99 {
		t.Fatalf("synonyms should agree: %v", c)
	}
	if c := Cosine(v[0], v[2]); c > 0.5 {
		t.Fatalf("unrelated texts too close: %v", c)
	}
	if Cosine(v[0], v[0][:3]) != 0 || Cosine(nil, nil) != 0 {
		t.Fatal("dimension mismatch must be 0")
	}
}

func TestOpenAIWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "nope", 404)
			return
		}
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		type d struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []d
		for i := range in.Input {
			data = append(data, d{Index: len(in.Input) - 1 - i, Embedding: []float32{float32(len(in.Input) - 1 - i), 1}}) // out of order on purpose
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": in.Model})
	}))
	defer srv.Close()
	o := &OpenAI{BaseURL: srv.URL + "/v1", ModelName: "m", APIKey: "k", HTTP: srv.Client()}
	v, err := o.Embed(context.Background(), []string{"a", "b"})
	if err != nil || len(v) != 2 || v[0][0] != 0 || v[1][0] != 1 {
		t.Fatalf("%v %v", v, err)
	}
	o.BaseURL = srv.URL + "/wrong"
	if _, err := o.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("HTTP error not reported")
	}
}

// Only an input error is permanent: the endpoint refusing the text itself. Overload, outage and timeouts are not.
func TestPermanentErrors(t *testing.T) {
	for status, want := range map[int]bool{400: true, 413: true, 422: true, 401: false, 403: false, 404: false, 405: false, 408: false, 425: false, 429: false, 500: false, 502: false, 503: false} {
		if got := Permanent(&StatusError{Status: status}); got != want {
			t.Errorf("HTTP %d: permanent %v, want %v", status, got, want)
		}
	}
	if Permanent(errors.New("connection refused")) || Permanent(fmt.Errorf("wrapped: %w", &StatusError{Status: 503})) || !Permanent(fmt.Errorf("wrapped: %w", &StatusError{Status: 413})) {
		t.Error("wrapping or plain errors")
	}
}

func TestRefused(t *testing.T) {
	for status, want := range map[int]bool{401: true, 403: true, 404: true, 405: true, 400: false, 413: false, 422: false, 429: false, 408: false, 500: false} {
		if got := Refused(&StatusError{Status: status}); got != want {
			t.Errorf("HTTP %d: refused %v, want %v", status, got, want)
		}
	}
}
