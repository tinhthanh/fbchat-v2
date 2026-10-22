package main

import (
	"os"
	"regexp"
	"testing"

	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"
)

// The Python library must present the same browser as the bridge: Facebook
// treats one session switching User-Agent or OS as a stolen session. The
// Python test needs the meta submodule, which only this job checks out.
func TestPythonBrowserIdentityMatchesBridge(t *testing.T) {
	src, err := os.ReadFile("../src/_core/_utils.py")
	if err != nil {
		t.Fatal(err)
	}
	find := func(pattern string) string {
		m := regexp.MustCompile(pattern).FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in src/_core/_utils.py", pattern)
		}
		return string(m[1])
	}
	version := find(`(?m)^BROWSER_CHROME_VERSION = "([^"]+)"`)
	fullSuffix := find(`(?m)^BROWSER_CHROME_VERSION_FULL = f"\{BROWSER_CHROME_VERSION\}([^"]+)"`)
	platform := find(`(?m)^BROWSER_PLATFORM = '([^']+)'`)
	if version != useragent.ChromeVersion {
		t.Errorf("Python Chrome %s, bridge %s", version, useragent.ChromeVersion)
	}
	if version+fullSuffix != useragent.ChromeVersionFull {
		t.Errorf("Python full version %s, bridge %s", version+fullSuffix, useragent.ChromeVersionFull)
	}
	if platform != useragent.SecCHPlatform {
		t.Errorf("Python platform %s, bridge %s", platform, useragent.SecCHPlatform)
	}
}
