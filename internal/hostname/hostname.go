// Package hostname resolves the host identity the agent reports to Datadog,
// in the stock Agent's order: an explicit config value, a hostname file, GCE
// instance metadata, the OS hostname, and the EC2 instance id only when the OS
// hostname looks like an EC2 default (ip-10-0-0-1 and friends), so a customized
// name survives. Azure instance names are not consulted, matching the stock
// default (azure_hostname_style "os").
package hostname

import (
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Resolve returns the hostname to report. configured and filePath come from the
// hostname and hostname_file config keys. Either may be empty.
func Resolve(configured, filePath string) string {
	if configured != "" {
		return configured
	}
	if filePath != "" {
		if b, err := os.ReadFile(filePath); err == nil {
			if h := strings.TrimSpace(string(b)); h != "" {
				return h
			}
		}
	}
	gceName, ec2Name := cloud()
	if gceName != "" {
		return gceName
	}
	osName, _ := os.Hostname()
	if osName != "" && !isDefaultHostname(osName) {
		return osName
	}
	if ec2Name != "" {
		return ec2Name
	}
	if osName != "" {
		return osName
	}
	return "unknown"
}

// isDefaultHostname reports whether h is a name the platform handed out rather
// than one an operator chose: empty, localhost, or one of EC2's generated
// prefixes (the stock Agent's ec2.IsDefaultHostname set). Only such names give
// way to the EC2 instance id.
func isDefaultHostname(h string) bool {
	h = strings.ToLower(h)
	if h == "" || h == "localhost" || h == "localhost.localdomain" {
		return true
	}
	for _, p := range []string{"ip-", "domu", "ec2amaz-"} {
		if strings.HasPrefix(h, p) {
			return true
		}
	}
	return false
}

// cloud probes the GCE and EC2 metadata services concurrently under a single
// short deadline. On a non-cloud host both probes simply fail within it.
func cloud() (gceName, ec2Name string) {
	client := &http.Client{Timeout: 400 * time.Millisecond}
	gc := make(chan string, 1)
	ec := make(chan string, 1)
	go func() { gc <- gce(client) }()
	go func() { ec <- ec2(client) }()
	return <-gc, <-ec
}

func ec2(client *http.Client) string {
	// IMDSv2: fetch a token, then the instance id. A missing token still works
	// where IMDSv1 is allowed.
	var token string
	if req, err := http.NewRequest(http.MethodPut, "http://169.254.169.254/latest/api/token", nil); err == nil {
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
		token = strings.TrimSpace(get(client, req))
	}
	req, err := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/instance-id", nil)
	if err != nil {
		return ""
	}
	if token != "" {
		req.Header.Set("X-aws-ec2-metadata-token", token)
	}
	return strings.TrimSpace(get(client, req))
}

func gce(client *http.Client) string {
	req, err := http.NewRequest(http.MethodGet, "http://metadata.google.internal/computeMetadata/v1/instance/hostname", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	return strings.TrimSpace(get(client, req))
}

// get performs req and returns the body on a 2xx response, or "" on any failure.
func get(client *http.Client, req *http.Request) string {
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return ""
	}
	return string(b)
}
