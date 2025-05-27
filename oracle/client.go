package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"

	slog "log/slog"
)

const (
	BillyTokenID   = "9Rhbn9G5poLvgnFzuYBtJgbzmiipNra35QpnUek9virt"
	SolWrappedID   = "So11111111111111111111111111111111111111112"
	DexPairAddress = "7EdQQSdkGvir2FMnuSsriqsksz44Cqf3fR7yRbbX6nX4"
)

// PriceData holds the three key price relationships between BILLY, SOL, and USD.
type PriceData struct {
	BillySol float64 // how many SOL per 1 BILLY (formerly Spot)
	BillyUSD float64 // how many USD per 1 BILLY (formerly USD)
	SolUSD   float64 // how many USD per 1 SOL
}

// PriceClient fetches prices from multiple upstreams, requiring an API key for Coingecko.
// PriceClient fetches prices from multiple upstreams, supporting demo and pro Coingecko API keys.
type PriceClient struct {
	http             *http.Client
	coingeckoAPIKey  string
	coingeckoKeyType string // "demo" or "pro"
}

// jupiterResponse models the new Price API v2 response.
type jupiterResponse struct {
	Data map[string]struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Price string `json:"price"`
	} `json:"data"`
}

// ErrNotReady indicates all upstreams have been down for too long.
var ErrNotReady = errors.New("service not ready: all price sources down >120s")

// NewPriceClient returns a PriceClient with default timeout and Coingecko API key.
// NewPriceClient returns a PriceClient with default timeout and Coingecko API key/type.
func NewPriceClient(httpClient *http.Client, key, keyType string) *PriceClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &PriceClient{http: httpClient, coingeckoAPIKey: key, coingeckoKeyType: keyType}
}

// FetchJupiter retrieves the BILLY/SOL spot price using the Jupiter v2 API.
func (pc *PriceClient) FetchJupiter(ctx context.Context) (PriceData, error) {
	// Query for both tokens, but we only care about BILLY relative to SOL
	url := fmt.Sprintf("https://lite-api.jup.ag/price/v2?ids=%s,%s&vsToken=%s", BillyTokenID, SolWrappedID, SolWrappedID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Error("creating Jupiter request", "error", err)
		return PriceData{}, err
	}

	resp, err := pc.http.Do(req)
	if err != nil {
		slog.Error("Jupiter API request failed", "error", err)
		return PriceData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("Jupiter API non-200 response", "status", resp.StatusCode, "body", string(body))
		return PriceData{}, fmt.Errorf("jupiter API returned %d", resp.StatusCode)
	}

	var jr jupiterResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		slog.Error("decoding Jupiter response", "error", err)
		return PriceData{}, err
	}

	entry, ok := jr.Data[BillyTokenID]
	if !ok {
		return PriceData{}, fmt.Errorf("jupiter response missing id %s", BillyTokenID)
	}
	price, err := strconv.ParseFloat(entry.Price, 64)
	if err != nil {
		slog.Error("parsing Jupiter price", "error", err, "price", entry.Price)
		return PriceData{}, err
	}

	// Populate the new PriceData struct:
	return PriceData{
		BillySol: price, // what Jupiter gave us
		BillyUSD: 0,     // not provided here
		SolUSD:   0,     // not provided here
	}, nil
}

// FetchCoingecko retrieves the BILLY/SOL spot price by querying both tokens vs USD in a single call and dividing.
func (pc *PriceClient) FetchCoingecko(ctx context.Context) (PriceData, error) {
	// Determine base URL and header key based on key type
	baseURL := "https://api.coingecko.com/api/v3/simple/price"
	headerName := ""
	if pc.coingeckoKeyType != "" {
		switch pc.coingeckoKeyType {
		case "pro":
			baseURL = "https://pro-api.coingecko.com/api/v3/simple/price"
			headerName = "x-cg-pro-api-key"
		case "demo":
			baseURL = "https://api.coingecko.com/api/v3/simple/price"
			headerName = "x-cg-demo-api-key"
		}
	}
	// Query both BILLY and SOLANA vs USD together
	url := fmt.Sprintf("%s?ids=%s,%s&vs_currencies=usd", baseURL, "billy-bets-by-virtuals", "solana")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Error("creating Coingecko request", "error", err)
		return PriceData{}, err
	}
	if pc.coingeckoAPIKey != "" {
		req.Header.Set(headerName, pc.coingeckoAPIKey)
	}

	resp, err := pc.http.Do(req)
	if err != nil {
		slog.Error("Coingecko API request failed", "error", err)
		return PriceData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("Coingecko non-200 response", "status", resp.StatusCode, "body", string(body))
		return PriceData{}, fmt.Errorf("coingecko returned %d", resp.StatusCode)
	}

	// Response format: {"billy-bets-by-virtuals":{"usd":...},"solana":{"usd":...}}
	var data map[string]map[string]float64
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		slog.Error("decoding Coingecko response", "error", err)
		return PriceData{}, err
	}

	billyUSD, ok1 := data["billy-bets-by-virtuals"]["usd"]
	solUSD, ok2 := data["solana"]["usd"]
	if !ok1 || !ok2 || solUSD == 0 {
		return PriceData{}, fmt.Errorf("invalid Coingecko data: %+v", data)
	}

	// Build and return the enriched PriceData
	return PriceData{
		BillySol: billyUSD / solUSD, // BILLY→SOL ratio
		BillyUSD: billyUSD,          // BILLY→USD price
		SolUSD:   solUSD,            // SOL→USD price
	}, nil
}

// FetchDexScreener retrieves BILLY/SOL spot and USD price from DexScreener using the pair address.
func (pc *PriceClient) FetchDexScreener(ctx context.Context) (PriceData, error) {
	// Query DexScreener by pairAddress
	url := fmt.Sprintf("https://api.dexscreener.com/latest/dex/search?q=%s", DexPairAddress)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Error("creating DexScreener request", "error", err)
		return PriceData{}, err
	}

	resp, err := pc.http.Do(req)
	if err != nil {
		slog.Error("DexScreener API request failed", "error", err)
		return PriceData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("DexScreener non-200 response", "status", resp.StatusCode, "body", string(body))
		return PriceData{}, fmt.Errorf("dexscreener returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("reading DexScreener response", "error", err)
		return PriceData{}, err
	}

	// Extract priceNative and priceUsd from pairs[0]
	vNative := gjson.GetBytes(data, "pairs.0.priceNative")
	vUsd := gjson.GetBytes(data, "pairs.0.priceUsd")
	if !vNative.Exists() || !vUsd.Exists() {
		return PriceData{}, fmt.Errorf("dexscreener: missing price fields")
	}

	native, err := strconv.ParseFloat(vNative.String(), 64)
	if err != nil {
		slog.Error("parsing priceNative", "error", err, "raw", vNative.String())
		return PriceData{}, err
	}
	usd, err := strconv.ParseFloat(vUsd.String(), 64)
	if err != nil {
		slog.Error("parsing priceUsd", "error", err, "raw", vUsd.String())
		return PriceData{}, err
	}

	// Compute SOL→USD = (BILLY→USD) / (BILLY→SOL)
	var solUsd float64
	if native != 0 {
		solUsd = usd / native
	}

	return PriceData{
		BillySol: native, // BILLY→SOL
		BillyUSD: usd,    // BILLY→USD
		SolUSD:   solUsd, // SOL→USD
	}, nil
}
