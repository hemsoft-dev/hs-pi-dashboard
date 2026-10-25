package usage

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Synthetic schema fixtures, not captured provider responses. See
// testdata/antigravity/README.md for the source contract and limitations.
func TestAntigravityGeminiOnlyMixedPools(t *testing.T) {
	short := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	weekly := time.Now().Add(96 * time.Hour).UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"groups":[{"name":"Claude","buckets":[
		{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"resetTime":%q},
		{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":%q},
		{"bucketId":"3p-5h","window":"5h","remainingFraction":0.1,"resetTime":%q},
		{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.08,"resetTime":%q},
		{"bucketId":"gemini-argon-wtu-weekly","window":"weekly","remainingFraction":0.3,"resetTime":%q}
	]}]}`, short.Format(time.RFC3339), weekly.Format(time.RFC3339), short.Add(time.Hour).Format(time.RFC3339), weekly.Add(time.Hour).Format(time.RFC3339), weekly.Add(2*time.Hour).Format(time.RFC3339))
	got, err := parseAntigravityWindows([]byte(body))
	if err != nil || len(got) != 2 {
		t.Fatalf("Gemini windows = %+v, %v", got, err)
	}
	if *got[0].UsedPercent != 10 || !got[0].ResetAt.Equal(short) || *got[1].UsedPercent != 20 || !got[1].ResetAt.Equal(weekly) {
		t.Fatal("unrelated values or resets selected", got)
	}
	if got[1].ProjectedEnd == nil || *got[1].ProjectedEnd < 46 || *got[1].ProjectedEnd > 47 {
		t.Fatal("forecast must use Gemini weekly usage and reset", got[1])
	}
}

func TestAntigravityInvalidGeminiDoesNotHideOtherValidWindow(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	valid := fmt.Sprintf(`{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"resetTime":%q}`, reset)
	for name, bad := range map[string]string{
		"missing":                      ``,
		"disabled":                     `"disabled":true,`,
		"missing fraction":             `"remainingFraction":null,`,
		"negative":                     `"remainingFraction":-0.1,`,
		"over one":                     `"remainingFraction":1.1,`,
		"wrong type":                   `"remainingFraction":"0.9",`,
		"bad reset":                    `"resetTime":"not-a-time",`,
		"expired":                      `"resetTime":"2000-01-01T00:00:00Z",`,
		"wrong window":                 `"window":"weekly",`,
		"ambiguous identity":           `"id":"3p-5h",`,
		"ambiguous fraction":           `"remaining_fraction":0.1,`,
		"ambiguous reset":              `"reset_time":"2000-01-01T00:00:00Z",`,
		"duplicate":                    `duplicate`,
		"malformed duplicate":          `malformed duplicate`,
		"malformed identity duplicate": `malformed identity duplicate`,
	} {
		t.Run(name, func(t *testing.T) {
			var candidate string
			switch name {
			case "missing":
			case "duplicate":
				candidate = valid + "," + valid
			case "malformed duplicate":
				candidate = valid + `,{"bucketId":"gemini-5h","remainingFraction":"invalid"}`
			case "malformed identity duplicate":
				candidate = valid + `,{"bucketId":"gemini-5h","id":3}`
			default:
				// Insert overrides after identity; JSON object keys remain unique.
				var base map[string]any
				if err := json.Unmarshal([]byte(valid), &base); err != nil {
					t.Fatal(err)
				}
				var overrides map[string]any
				if err := json.Unmarshal([]byte("{"+strings.TrimSuffix(bad, ",")+"}"), &overrides); err != nil {
					t.Fatal(err)
				}
				for k, v := range overrides {
					base[k] = v
				}
				b, err := json.Marshal(base)
				if err != nil {
					t.Fatal(err)
				}
				candidate = string(b)
			}
			weekly := fmt.Sprintf(`{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.8,"reset_time":%q}`, time.Now().Add(96*time.Hour).UTC().Format(time.RFC3339))
			unrelated := fmt.Sprintf(`{"bucketId":"3p-5h","window":"5h","remainingFraction":0.1,"resetTime":%q}`, reset)
			makeBody := func(other string) []byte {
				buckets := []string{unrelated}
				if candidate != "" {
					buckets = append(buckets, candidate)
				}
				if other != "" {
					buckets = append(buckets, other)
				}
				return []byte(`{"groups":[{"name":"Gemini Models","buckets":[` + strings.Join(buckets, ",") + `]}]}`)
			}
			got, err := parseAntigravityWindows(makeBody(weekly))
			if err != nil || len(got) != 1 || got[0].BucketID != "gemini-weekly" || *got[0].UsedPercent != 20 {
				t.Fatal(got, err)
			}
			got, err = parseAntigravityWindows(makeBody(""))
			if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "Gemini Models quota unavailable") {
				t.Fatal(got, err)
			}
		})
	}
}

func TestAntigravityMalformedOtherPoolCannotPoisonGemini(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"groups":[{"buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":1,"reset_time":%q},{"id":"3p-weekly","remaining_fraction":"invalid"}]}]}`, reset)
	got, err := parseAntigravityWindows([]byte(body))
	if err != nil || len(got) != 1 || got[0].BucketID != "gemini-5h" || *got[0].UsedPercent != 0 {
		t.Fatal(got, err)
	}
}
