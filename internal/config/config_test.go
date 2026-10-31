package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"LAKE_LOCAL_ROOT": "/lake", "LAKE_TEMP_DIR": "/scratch"}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Storage: StorageLocal, LocalRoot: "/lake", DuckDBMemory: "1GB",
		GDELTBaseURL: DefaultGDELTBaseURL, TempDir: "/scratch"}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadAddsTrailingSlashToBaseURL(t *testing.T) {
	c, err := Load(env(map[string]string{"LAKE_LOCAL_ROOT": "/lake", "LAKE_GDELT_BASE_URL": "http://fake:8080/gdeltv2"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.GDELTBaseURL != "http://fake:8080/gdeltv2/" {
		t.Fatalf("base URL %q", c.GDELTBaseURL)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"local without root": {map[string]string{}, "LAKE_LOCAL_ROOT is required"},
		"unknown backend":    {map[string]string{"LAKE_STORAGE": "floppy"}, `"floppy" is not supported`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
