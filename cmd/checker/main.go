package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Apple 認定整備品APIのレスポンス構造
type AppleResponse struct {
	Products []Product `json:"products"`
}

type Product struct {
	PartNumber  string `json:"partNumber"`
	ProductName string `json:"productName"`
	Price       struct {
		CurrentPrice float64 `json:"currentPrice"`
	} `json:"price"`
	URL string `json:"productDetailsPageURL"`
}

// 監視対象のフィルター条件
var (
	targetModels    = []string{"iPhone 15", "iPhone 16"}
	targetStorages  = []string{"128GB", "256GB", "512GB"}
	appleRefurbURL  = "https://www.apple.com/jp/shop/product-locator-meta?family=iphone&fts=refurbished"
)

func main() {
	lineToken := os.Getenv("LINE_CHANNEL_ACCESS_TOKEN")
	lineUserID := os.Getenv("LINE_USER_ID")
	seenFile := os.Getenv("SEEN_FILE")
	if seenFile == "" {
		seenFile = "seen.json"
	}

	if lineToken == "" || lineUserID == "" {
		log.Fatal("LINE_CHANNEL_ACCESS_TOKEN と LINE_USER_ID を環境変数に設定してください")
	}

	// 既通知済みの商品IDを読み込む
	seen := loadSeen(seenFile)

	// Apple APIから商品一覧を取得
	products, err := fetchProducts()
	if err != nil {
		log.Fatalf("商品取得失敗: %v", err)
	}
	log.Printf("取得した商品数: %d", len(products))

	// フィルタリング
	matched := filterProducts(products)
	log.Printf("条件に一致した商品数: %d", len(matched))

	// 新規分だけ通知
	newCount := 0
	for _, p := range matched {
		if seen[p.PartNumber] {
			log.Printf("スキップ（既通知）: %s", p.ProductName)
			continue
		}
		log.Printf("新規通知: %s", p.ProductName)
		if err := sendLINE(lineToken, lineUserID, p); err != nil {
			log.Printf("LINE通知失敗 %s: %v", p.PartNumber, err)
			continue
		}
		seen[p.PartNumber] = true
		newCount++
	}

	log.Printf("新規通知数: %d", newCount)

	// 既通知リストを保存
	if err := saveSeen(seenFile, seen); err != nil {
		log.Fatalf("seen.json 保存失敗: %v", err)
	}
}

func fetchProducts() ([]Product, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", appleRefurbURL, nil)
	if err != nil {
		return nil, err
	}
	// Appleのページに通常のブラウザとして見せる
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "ja-JP,ja;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTPステータス: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result AppleResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("JSONパース失敗: %v\nボディ: %s", err, string(body[:min(500, len(body))]))
	}

	return result.Products, nil
}

func filterProducts(products []Product) []Product {
	var matched []Product
	modelRe := buildRegexp(targetModels)
	storageRe := buildRegexp(targetStorages)

	for _, p := range products {
		name := p.ProductName
		if modelRe.MatchString(name) && storageRe.MatchString(name) {
			matched = append(matched, p)
		}
	}
	return matched
}

func buildRegexp(patterns []string) *regexp.Regexp {
	escaped := make([]string, len(patterns))
	for i, p := range patterns {
		escaped[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(strings.Join(escaped, "|"))
}

func sendLINE(token, userID string, p Product) error {
	productURL := fmt.Sprintf("https://www.apple.com%s", p.URL)
	message := fmt.Sprintf(
		"🍎 認定整備品 入荷通知\n\n📱 %s\n💴 ¥%s\n🔗 %s",
		p.ProductName,
		formatPrice(p.Price.CurrentPrice),
		productURL,
	)

	body := map[string]interface{}{
		"to": userID,
		"messages": []map[string]string{
			{
				"type": "text",
				"text": message,
			},
		},
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.line.me/v2/bot/message/push", bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("LINE APIエラー %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func formatPrice(price float64) string {
	s := fmt.Sprintf("%.0f", price)
	// 3桁区切り
	n := len(s)
	if n <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (n-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

func loadSeen(path string) map[string]bool {
	seen := make(map[string]bool)
	data, err := os.ReadFile(path)
	if err != nil {
		return seen // ファイルがなければ空で返す
	}
	_ = json.Unmarshal(data, &seen)
	return seen
}

func saveSeen(path string, seen map[string]bool) error {
	data, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}