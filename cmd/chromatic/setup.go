package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultPort             = "3000"
	defaultDatabasePath     = "/data/chromatic.db"
	defaultUploadPath       = "/data/files"
	defaultLogoPath         = "/data/logos"
	defaultChromaticImage   = "ghcr.io/davidtorcivia/chromatic:latest"
	defaultPublicURLProd    = "https://stream.yourdomain.com"
	defaultPublicURLDev     = "http://localhost:3000"
	defaultTurnRealmProd    = "stream.yourdomain.com"
	defaultTurnRealmDev     = "localhost"
	defaultEnvRelativePath  = "deployments/.env"
	defaultAllowedOrigins   = "https://stream.yourdomain.com"
	defaultTurnMode         = "external"
	defaultExternalTurnURLs = "turn:turn.cloudflare.com:3478?transport=udp,turn:turn.cloudflare.com:3478?transport=tcp,turns:turn.cloudflare.com:443?transport=tcp"
	defaultExternalTurnUser = ""
	defaultExternalTurnPass = ""
)

func runSetup() int {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Chromatic setup wizard")
	fmt.Println("This will create a .env file for Docker or local deployment.")
	fmt.Println("")

	envPath := defaultEnvRelativePath
	if !promptYesNo(reader, "Write config to deployments/.env", true) {
		envPath = prompt(reader, "Enter .env path", envPath)
	}
	envPath = strings.TrimSpace(envPath)
	if envPath == "" {
		fmt.Println("No path provided. Aborting.")
		return 1
	}

	if _, err := os.Stat(envPath); err == nil {
		if !promptYesNo(reader, "File exists. Overwrite", false) {
			fmt.Println("Setup cancelled.")
			return 1
		}
	}

	productionMode := promptYesNo(reader, "Production mode", true)

	publicURLDefault := defaultPublicURLProd
	if !productionMode {
		publicURLDefault = defaultPublicURLDev
	}
	publicURL := requireValue(reader, "PUBLIC_URL", publicURLDefault)

	adminToken := promptSecret(reader, "ADMIN_TOKEN", true)

	turnMode := promptChoice(reader, "TURN_MODE", []string{"external", "hybrid", "self-hosted"}, defaultTurnMode)

	turnRealm := ""
	turnSecret := ""
	publicIP := ""
	if turnMode != "external" {
		turnRealmDefault := defaultTurnRealmProd
		if !productionMode {
			turnRealmDefault = defaultTurnRealmDev
		}
		if host := hostFromURL(publicURL); host != "" {
			turnRealmDefault = host
		}
		turnRealm = requireValue(reader, "TURN_REALM", turnRealmDefault)
		turnSecret = promptSecret(reader, "TURN_SECRET", true)
		publicIP = prompt(reader, "PUBLIC_IP (required for self-hosted TURN)", "")
	}

	allowedOrigins := ""
	if productionMode {
		defaultOrigins := defaultAllowedOrigins
		if publicURL != "" {
			defaultOrigins = publicURL
		}
		allowedOrigins = requireValue(reader, "ALLOWED_ORIGINS (comma-separated)", defaultOrigins)
	}

	trustedProxies := prompt(reader, "TRUSTED_PROXIES (optional)", "")
	chromaticImage := prompt(reader, "CHROMATIC_IMAGE", defaultChromaticImage)

	turnCloudflareKeyID := ""
	turnCloudflareAPIToken := ""
	externalTurnURLs := ""
	externalTurnUser := defaultExternalTurnUser
	externalTurnPass := defaultExternalTurnPass

	if turnMode != "self-hosted" {
		if promptYesNo(reader, "Use Cloudflare TURN credentials (recommended)", true) {
			turnCloudflareKeyID = requireValue(reader, "TURN_CLOUDFLARE_KEY_ID", "")
			turnCloudflareAPIToken = requireValue(reader, "TURN_CLOUDFLARE_API_TOKEN", "")
			externalTurnURLs = defaultExternalTurnURLs
		}

		configureStatic := promptYesNo(reader, "Configure static external TURN credentials", false)
		if turnMode == "external" && turnCloudflareKeyID == "" && turnCloudflareAPIToken == "" {
			configureStatic = true
		}

		if configureStatic {
			externalTurnURLs = requireValue(reader, "TURN_EXTERNAL_URLS (comma-separated)", externalTurnURLs)
			externalTurnUser = requireValue(reader, "TURN_EXTERNAL_USER", "")
			externalTurnPass = requireValue(reader, "TURN_EXTERNAL_PASS", "")
		}
	}

	content := buildEnvFile(envFileParams{
		adminToken:          adminToken,
		publicURL:           publicURL,
		turnMode:            turnMode,
		turnSecret:          turnSecret,
		turnRealm:           turnRealm,
		productionMode:      productionMode,
		allowedOrigins:      allowedOrigins,
		trustedProxies:      trustedProxies,
		chromaticImage:      chromaticImage,
		databasePath:        defaultDatabasePath,
		uploadPath:          defaultUploadPath,
		logoPath:            defaultLogoPath,
		port:                defaultPort,
		publicIP:            publicIP,
		turnExternalURLs:    externalTurnURLs,
		turnExternalUser:    externalTurnUser,
		turnExternalPass:    externalTurnPass,
		turnCloudflareKeyID: turnCloudflareKeyID,
		turnCloudflareToken: turnCloudflareAPIToken,
	})

	if err := os.MkdirAll(filepath.Dir(envPath), 0755); err != nil {
		fmt.Printf("Failed to create directory: %v\n", err)
		return 1
	}

	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		fmt.Printf("Failed to write %s: %v\n", envPath, err)
		return 1
	}

	fmt.Println("")
	fmt.Printf("Wrote %s\n", envPath)
	fmt.Println("Restart Chromatic after applying the new environment.")
	return 0
}

type envFileParams struct {
	adminToken          string
	publicURL           string
	turnMode            string
	turnSecret          string
	turnRealm           string
	productionMode      bool
	allowedOrigins      string
	trustedProxies      string
	chromaticImage      string
	databasePath        string
	uploadPath          string
	logoPath            string
	port                string
	publicIP            string
	turnExternalURLs    string
	turnExternalUser    string
	turnExternalPass    string
	turnCloudflareKeyID string
	turnCloudflareToken string
}

func buildEnvFile(p envFileParams) string {
	var b strings.Builder
	writeLine := func(line string) {
		b.WriteString(line)
		b.WriteString("\n")
	}

	writeLine("# Chromatic Environment Configuration")
	writeLine("# Generated by `chromatic setup`")
	writeLine("")
	writeLine("# =============================================================================")
	writeLine("# REQUIRED SETTINGS")
	writeLine("# =============================================================================")
	writeLine(fmt.Sprintf("ADMIN_TOKEN=%s", p.adminToken))
	writeLine(fmt.Sprintf("PUBLIC_URL=%s", p.publicURL))
	writeLine(fmt.Sprintf("TURN_MODE=%s", p.turnMode))
	writeLine(fmt.Sprintf("TURN_SECRET=%s", p.turnSecret))
	writeLine(fmt.Sprintf("TURN_REALM=%s", p.turnRealm))
	writeLine("")
	writeLine("# =============================================================================")
	writeLine("# PRODUCTION SETTINGS")
	writeLine("# =============================================================================")
	writeLine(fmt.Sprintf("PRODUCTION_MODE=%t", p.productionMode))
	if p.allowedOrigins != "" {
		writeLine(fmt.Sprintf("ALLOWED_ORIGINS=%s", p.allowedOrigins))
	} else {
		writeLine("ALLOWED_ORIGINS=")
	}
	writeLine(fmt.Sprintf("TRUSTED_PROXIES=%s", p.trustedProxies))
	writeLine(fmt.Sprintf("CHROMATIC_IMAGE=%s", p.chromaticImage))
	writeLine("")
	writeLine("# =============================================================================")
	writeLine("# STORAGE PATHS (Docker volume mounts)")
	writeLine("# =============================================================================")
	writeLine(fmt.Sprintf("DATABASE_PATH=%s", p.databasePath))
	writeLine(fmt.Sprintf("UPLOAD_PATH=%s", p.uploadPath))
	writeLine(fmt.Sprintf("LOGO_PATH=%s", p.logoPath))
	writeLine("")
	writeLine("# =============================================================================")
	writeLine("# SERVER SETTINGS")
	writeLine("# =============================================================================")
	writeLine(fmt.Sprintf("PORT=%s", p.port))
	writeLine("# Required only when TURN_MODE=self-hosted or TURN_MODE=hybrid")
	writeLine(fmt.Sprintf("PUBLIC_IP=%s", p.publicIP))
	writeLine("")
	writeLine("# =============================================================================")
	writeLine("# EXTERNAL TURN PROVIDERS")
	writeLine("# =============================================================================")
	writeLine("# Cloudflare TURN (recommended for TURN_MODE=external)")
	writeLine(fmt.Sprintf("TURN_CLOUDFLARE_KEY_ID=%s", p.turnCloudflareKeyID))
	writeLine(fmt.Sprintf("TURN_CLOUDFLARE_API_TOKEN=%s", p.turnCloudflareToken))
	writeLine("# Optional static external TURN credentials")
	writeLine(fmt.Sprintf("TURN_EXTERNAL_URLS=%s", p.turnExternalURLs))
	writeLine(fmt.Sprintf("TURN_EXTERNAL_URL=%s", firstCSVValue(p.turnExternalURLs)))
	writeLine(fmt.Sprintf("TURN_EXTERNAL_USER=%s", p.turnExternalUser))
	writeLine(fmt.Sprintf("TURN_EXTERNAL_PASS=%s", p.turnExternalPass))
	return b.String()
}

func prompt(reader *bufio.Reader, label, defaultValue string) string {
	if defaultValue != "" {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	} else {
		fmt.Printf("%s: ", label)
	}
	text, _ := reader.ReadString('\n')
	text = strings.TrimSpace(text)
	if text == "" {
		return defaultValue
	}
	return text
}

func requireValue(reader *bufio.Reader, label, defaultValue string) string {
	for {
		value := prompt(reader, label, defaultValue)
		if strings.TrimSpace(value) != "" {
			return value
		}
		fmt.Println("Value is required.")
	}
}

func promptYesNo(reader *bufio.Reader, label string, defaultYes bool) bool {
	suffix := "y/N"
	if defaultYes {
		suffix = "Y/n"
	}
	for {
		fmt.Printf("%s (%s): ", label, suffix)
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(strings.ToLower(text))
		if text == "" {
			return defaultYes
		}
		if text == "y" || text == "yes" {
			return true
		}
		if text == "n" || text == "no" {
			return false
		}
		fmt.Println("Please enter y or n.")
	}
}

func promptChoice(reader *bufio.Reader, label string, choices []string, defaultChoice string) string {
	if len(choices) == 0 {
		return defaultChoice
	}

	display := strings.Join(choices, "/")
	for {
		fmt.Printf("%s (%s) [%s]: ", label, display, defaultChoice)
		text, _ := reader.ReadString('\n')
		text = strings.TrimSpace(strings.ToLower(text))
		if text == "" {
			return defaultChoice
		}
		for _, choice := range choices {
			if text == strings.ToLower(choice) {
				return choice
			}
		}
		fmt.Printf("Please enter one of: %s\n", display)
	}
}

func promptSecret(reader *bufio.Reader, label string, allowGenerate bool) string {
	if allowGenerate && promptYesNo(reader, fmt.Sprintf("Generate %s", label), true) {
		token, err := generateHex(32)
		if err != nil {
			fmt.Printf("Failed to generate %s: %v\n", label, err)
		} else {
			return token
		}
	}
	return requireValue(reader, label, "")
}

func generateHex(bytesCount int) (string, error) {
	b := make([]byte, bytesCount)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hostFromURL(raw string) string {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return ""
	}
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func firstCSVValue(values string) string {
	for _, candidate := range strings.Split(values, ",") {
		trimmed := strings.TrimSpace(candidate)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
