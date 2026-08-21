package templatehook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderIsBoundedAndMissingKeysFail(t *testing.T) {
	renderer := Renderer{MaxOutputBytes: 64}
	out, err := renderer.Render("server", []byte("hostname {{ cfgQuote .Inputs.hostname }}\nrcon_password {{ cfgQuote .Inputs.rcon }}\n"), map[string]Input{
		"hostname": PublicInput("FXVCS Dev"), "rcon": SecretInput("secret"),
	})
	if err != nil || !strings.Contains(string(out), "FXVCS Dev") {
		t.Fatalf("render: %q, %v", out, err)
	}
	if _, err := renderer.Render("missing", []byte(`{{.Inputs.absent}}`), map[string]Input{}); err == nil {
		t.Fatal("missing key rendered")
	}
	if _, err := (Renderer{MaxOutputBytes: 3}).Render("large", []byte(`1234`), nil); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("want output limit, got %v", err)
	}
}

func TestSecretInputsRequireCfgQuote(t *testing.T) {
	renderer := Renderer{MaxOutputBytes: 256}
	inputs := map[string]Input{"token": SecretInput(`quote" and \ slash`)}
	if _, err := renderer.Render("direct", []byte(`{{ .Inputs.token }}`), inputs); !errors.Is(err, ErrSecretEncoding) {
		t.Fatalf("direct secret rendered: %v", err)
	}
	if _, err := renderer.Render("index", []byte(`{{ index .Inputs "token" }}`), inputs); !errors.Is(err, ErrFunction) {
		t.Fatalf("non-allowlisted builtin accepted: %v", err)
	}
	out, err := renderer.Render("quoted", []byte(`set token {{ cfgQuote .Inputs.token }}`), inputs)
	if err != nil || string(out) != `set token "quote\" and \\ slash"` {
		t.Fatalf("quoted secret: %q %v", out, err)
	}
	inputs["token"] = SecretInput("line1\nline2")
	if _, err := renderer.Render("newline", []byte(`{{ cfgQuote .Inputs.token }}`), inputs); !errors.Is(err, ErrCfgValue) {
		t.Fatalf("newline secret accepted: %v", err)
	}
}

func TestWriteContainsOutputAndProtectsSecrets(t *testing.T) {
	realization := filepath.Join(t.TempDir(), "releases", "rz_test")
	options := WriteOptions{
		RealizationDir: realization, AllowedDestinationRoot: "runtime/config",
		Destination: "runtime/config/server.cfg", Mode: "0600", ContainsSecret: true,
	}
	path, err := Write(options, []byte("secret config"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret config" {
		t.Fatalf("written output: %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
	if _, err := Write(options, []byte("replace")); !errors.Is(err, ErrDestination) {
		t.Fatalf("existing destination replaced: %v", err)
	}
	options.Destination = "runtime/config/public.cfg"
	options.Mode = "0644"
	if _, err := Write(options, []byte("secret")); !errors.Is(err, ErrUnsafeMode) {
		t.Fatalf("world-readable secret accepted: %v", err)
	}
	options.Destination = "resources/leak.cfg"
	options.Mode = "0600"
	if _, err := Write(options, nil); err == nil {
		t.Fatal("resource-tree output accepted")
	}
}
