package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

type Config struct {
	Port           string
	MKApiURL       string
	MKAuthToken    string
	MKAuthPassword string
	APIKey         string
}

func loadConfig() *Config {
	_ = godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	return &Config{
		Port:           port,
		MKApiURL:       os.Getenv("MK_API_URL"),
		MKAuthToken:    os.Getenv("MK_AUTH_TOKEN"),
		MKAuthPassword: os.Getenv("MK_AUTH_PASSWORD"),
		APIKey:         os.Getenv("API_KEY"),
	}
}

type MKAuthResponse struct {
	Expire              string `json:"Expire"`
	LimiteUso           int    `json:"LimiteUso"`
	ServicosAutorizados []int  `json:"ServicosAutorizados"`
	Token               string `json:"Token"`
	Status              string `json:"status"`
}

type MKIntegrationService struct {
	cfg        *Config
	httpClient *http.Client

	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func NewMKIntegrationService(cfg *Config) *MKIntegrationService {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxConnsPerHost = 100
	t.MaxIdleConnsPerHost = 100

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: t,
	}

	return &MKIntegrationService{
		cfg:        cfg,
		httpClient: client,
	}
}

func (s *MKIntegrationService) Authenticate(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	if s.cachedToken != "" && time.Now().Before(s.tokenExpiry) {
		token := s.cachedToken
		s.tokenMu.Unlock()
		return token, nil
	}
	s.tokenMu.Unlock()

	url := fmt.Sprintf("%s/mk/WSAutenticacao.rule?sys=MK0&token=%s&cd_servico=9999", s.cfg.MKApiURL, s.cfg.MKAuthToken)
	if s.cfg.MKAuthPassword != "" {
		url += fmt.Sprintf("&password=%s", s.cfg.MKAuthPassword)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var authRes MKAuthResponse
	if err := json.Unmarshal(body, &authRes); err != nil {
		return "", fmt.Errorf("failed to parse auth response: %w. Body: %s", err, string(body))
	}

	if authRes.Token == "" {
		return "", fmt.Errorf("authentication failed, no token returned: %s", string(body))
	}

	s.tokenMu.Lock()
	s.cachedToken = authRes.Token
	s.tokenExpiry = time.Now().Add(8 * time.Minute) // Fallback conservador
	if authRes.Expire != "" {
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "01/02/2006 15:04:05"} {
			if t, parseErr := time.Parse(layout, authRes.Expire); parseErr == nil {
				s.tokenExpiry = t.Add(-2 * time.Minute)
				break
			}
		}
	}
	s.tokenMu.Unlock()

	return authRes.Token, nil
}

func (s *MKIntegrationService) FetchRawClientByDoc(ctx context.Context, sessionToken string, doc string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/mk/WSMKConsultaDoc.rule?sys=MK0&token=%s&doc=%s", s.cfg.MKApiURL, sessionToken, doc)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse client response: %w. Body: %s", err, string(body))
	}

	return raw, nil
}

func main() {
	cfg := loadConfig()
	if cfg.MKApiURL == "" || cfg.MKAuthToken == "" {
		log.Fatal("MK_API_URL and MK_AUTH_TOKEN must be set in .env")
	}

	mkService := NewMKIntegrationService(cfg)
	app := fiber.New()

	app.Get("/consulta", func(c *fiber.Ctx) error {
		key := c.Query("key")
		if cfg.APIKey != "" && key != cfg.APIKey {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "Unauthorized API Key"})
		}

		doc := c.Query("doc")
		if doc == "" {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Query parameter 'doc' (CPF/CNPJ) is required"})
		}

		ctx := context.Background()

		// 1. Authenticate / Get cached token
		sessionToken, err := mkService.Authenticate(ctx)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// 2. Fetch raw client data
		data, err := mkService.FetchRawClientByDoc(ctx, sessionToken, doc)
		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		// 3. Return JSON exactly as received
		return c.JSON(data)
	})

	log.Printf("Starting server on port %s", cfg.Port)
	if err := app.Listen(":" + cfg.Port); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}
