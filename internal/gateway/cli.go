package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func HandleCLI(args []string, stdout, stderr io.Writer) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "create-agent-secret":
		createAgentSecret(stdout, stderr)
		return true
	case "create-agent":
		createAgentSecret(stdout, stderr)
		return true
	case "create-key":
		createAPIKey(args[1:], stdout, stderr)
		return true
	case "help", "--help", "-h":
		printGatewayUsage(stdout)
		return true
	}
	return false
}

func createAgentSecret(stdout, stderr io.Writer) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(stderr, "create-agent-secret: %v\n", err)
		return
	}
	b, _ := json.MarshalIndent(map[string]string{
		"agent_public_key":  base64.RawURLEncoding.EncodeToString(publicKey),
		"agent_private_key": base64.RawURLEncoding.EncodeToString(privateKey),
	}, "", "  ")
	fmt.Fprintln(stdout, string(b))
}

func createAPIKey(args []string, stdout, stderr io.Writer) {
	name := ""
	caps := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--name":
			i++
			if i < len(args) {
				name = args[i]
			}
		case "--capabilities":
			i++
			if i < len(args) {
				for _, cap := range strings.Split(args[i], ",") {
					cap = strings.TrimSpace(cap)
					if cap != "" {
						caps = append(caps, cap)
					}
				}
			}
		}
	}
	if name == "" {
		fmt.Fprintln(stderr, "create-key: --name is required")
		return
	}
	key, err := randomToken(32)
	if err != nil {
		fmt.Fprintf(stderr, "create-key: %v\n", err)
		return
	}
	hash, err := hashSecret(key)
	if err != nil {
		fmt.Fprintf(stderr, "create-key: %v\n", err)
		return
	}
	out := map[string]any{"name": name, "api_key": key, "key_hash": hash, "capabilities": caps}
	if len(caps) == 1 && caps[0] == "*" {
		out["capabilities"] = "*"
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(stdout, string(b))
}

func hashSecret(secret string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func printGatewayUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  gateway                         start gateway server")
	fmt.Fprintln(w, "  gateway create-agent-secret     generate an Ed25519 agent keypair")
	fmt.Fprintln(w, "  gateway create-agent            alias for create-agent-secret")
	fmt.Fprintln(w, "  gateway create-key --name NAME --capabilities cap1,cap2")
}
