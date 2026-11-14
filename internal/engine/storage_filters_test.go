package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/skyri/ucd/internal/api"
)

func TestStoragePruneForwardsFilters(t *testing.T) {
	tests := []struct {
		scope, endpoint, key, value string
		all                         bool
		expected                    map[string]map[string]bool
	}{
		{"containers", "/containers/prune", "label", "fixture=unique", false, map[string]map[string]bool{"label": {"fixture=unique": true}}},
		{"images", "/images/prune", "label", "fixture=unique", true, map[string]map[string]bool{"label": {"fixture=unique": true}, "dangling": {"false": true}}},
		{"networks", "/networks/prune", "label", "fixture=unique", false, map[string]map[string]bool{"label": {"fixture=unique": true}}},
		{"volumes", "/volumes/prune", "label", "fixture=unique", true, map[string]map[string]bool{"label": {"fixture=unique": true}, "all": {"true": true}}},
		{"build-cache", "/build/prune", "id", "^owned-id$", true, map[string]map[string]bool{"id": {"^owned-id$": true}}},
	}
	for _, test := range tests {
		t.Run(test.scope, func(t *testing.T) {
			var seen bool
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.45")
					return
				}
				seen = true
				if got := strings.TrimPrefix(r.URL.Path, "/v1.45"); got != test.endpoint {
					t.Errorf("endpoint = %q, want %q", got, test.endpoint)
				}
				var got map[string]map[string]bool
				if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &got); err != nil {
					t.Errorf("decode filters: %v", err)
				} else if !reflect.DeepEqual(got, test.expected) {
					t.Errorf("filters = %#v, want %#v", got, test.expected)
				}
				fmt.Fprint(w, `{"ContainersDeleted":["owned"],"ImagesDeleted":[{"Deleted":"owned"}],"NetworksDeleted":["owned"],"VolumesDeleted":["owned"],"CachesDeleted":["owned"]}`)
			})
			params := api.StoragePruneParams{Scope: test.scope, All: test.all, Filters: map[string][]string{test.key: {test.value}}, Confirmed: true}
			if _, err := call(t, b, "storage.prune", params); err != nil {
				t.Fatal(err)
			}
			if !seen {
				t.Fatal("prune did not reach Docker")
			}
		})
	}
}

func TestStorageRejectsMisspelledFilterBeforeDocker(t *testing.T) {
	for _, method := range []string{"storage.preview", "storage.prune"} {
		t.Run(method, func(t *testing.T) {
			b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unknown field reached Docker: %s", r.URL)
			})
			_, err := call(t, b, method, map[string]any{
				"scope": "volumes", "all": true, "confirmed": true,
				"filter": map[string][]string{"label": {"fixture=owned"}},
			})
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != "invalid_argument" || !strings.Contains(apiErr.Message, "filter") {
				t.Fatalf("got %v, want invalid_argument for misspelled filter", err)
			}
		})
	}
}

func TestStoragePruneRejectsUnsafeFiltersBeforeDocker(t *testing.T) {
	tests := []struct {
		scope   string
		filters map[string][]string
	}{
		{"images", map[string][]string{"label": {}}},
		{"images", map[string][]string{"label": {" "}}},
		{"volumes", map[string][]string{"all": {"true"}}},
		{"networks", map[string][]string{"typo": {"owned"}}},
		{"containers", map[string][]string{"label!": {"stage=prod", "stage=staging"}}},
		{"images", map[string][]string{"label!": {"stage=prod", "stage=staging"}}},
		{"networks", map[string][]string{"label!": {"stage=prod", "stage=staging"}}},
		{"volumes", map[string][]string{"label!": {"stage=prod", "stage=staging"}}},
		{"build-cache", map[string][]string{"label": {"fixture=owned"}}},
		{"build-cache", map[string][]string{"parent": {"owned"}}},
		{"build-cache", map[string][]string{"inuse": {"false"}}},
		{"build-cache", map[string][]string{"shared": {"false"}}},
		{"build-cache", map[string][]string{"private": {"true"}}},
		{"build-cache", map[string][]string{"id": {"first", "second"}}},
	}
	for _, test := range tests {
		for _, method := range []string{"storage.preview", "storage.prune"} {
			t.Run(test.scope+"/"+method+"/"+fmt.Sprint(test.filters), func(t *testing.T) {
				b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/_ping" {
						w.Header().Set("API-Version", "1.45")
						return
					}
					t.Errorf("invalid filter reached Docker: %s", r.URL)
				})
				_, err := call(t, b, method, api.StoragePruneParams{Scope: test.scope, Filters: test.filters, Confirmed: true})
				if err == nil {
					t.Fatal("accepted invalid or unsafe prune filter")
				}
			})
		}
	}
}

func TestStoragePreviewExplainsAggregateWithFilters(t *testing.T) {
	b := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.45")
			return
		}
		fmt.Fprint(w, `{"Images":[],"Containers":[],"Volumes":[],"BuildCache":[]}`)
	})
	got, err := call(t, b, "storage.preview", api.StoragePruneParams{Scope: "images", Filters: map[string][]string{"label": {"fixture=unique"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.(api.StoragePreview).Description, "estimates are not filtered") {
		t.Fatalf("preview did not explain aggregate estimates: %#v", got)
	}
}
