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

	"github.com/PuerkitoBio/goquery"
)

type Product struct {
	PartNumber  string
	ProductName string
	URL         string
}

// JSON-LD 形式（HTMLから抽出される構造）
type JsonLDProduct struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Offers []struct {
		SKU string `json:"sku"`
	} `json:"offers"`
}

// 監視対象のフィルター条件
var (
	targetModels    = []string{"iPhone 15", "iPhone 16"}
	targetStorages  = []string{"128GB", "256GB", "512GB"}
	appleRefurbURL  = "https://www.apple.com/jp/shop/refurbished/iphone"
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

	// フィルタリング
	matched := filterProducts(products)

	// 新規分だけ通知
	newCount := 0
	for _, p := range matched {
		if seen[p.PartNumber] {
			continue
		}
		if err := sendLINE(lineToken, lineUserID, p); err != nil {
			log.Printf("LINE通知失敗 %s: %v", p.PartNumber, err)
			continue
		}
		seen[p.PartNumber] = true
		newCount++
	}

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
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
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

	// HTMLから JSON-LD を抽出 → Product に変換
	products, err := extractProducts(string(body))
	if err != nil {
		return nil, err
	}

	log.Printf("取得: %d件", len(products))
	return products, nil
}

// HTMLから JSON-LD スクリプトタグを抽出してProduct配列に変換
func extractProducts(html string) ([]Product, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		log.Printf("HTML パース失敗: %v", err)
		return nil, err
	}

	var products []Product
	
	doc.Find("script[type='application/ld+json']").Each(func(i int, s *goquery.Selection) {
		var j JsonLDProduct
		if err := json.Unmarshal([]byte(s.Text()), &j); err != nil {
			return
		}

		// name, url, sku があれば Product に追加
		if j.Name != "" && j.URL != "" && len(j.Offers) > 0 && j.Offers[0].SKU != "" {
			products = append(products, Product{
				PartNumber:  j.Offers[0].SKU,  // SKU を一意識別子として使用
				ProductName: j.Name,
				URL:         j.URL,
			})
		}
	})

	if len(products) == 0 {
		return nil, fmt.Errorf("商品が見つかりません")
	}

	return products, nil
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

	log.Printf("フィルタリング: %d件 → %d件", len(products), len(matched))
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
	// URLが既に完全形式か確認
	productURL := p.URL
	if !strings.HasPrefix(p.URL, "http") {
		productURL = fmt.Sprintf("https://www.apple.com%s", p.URL)
	}
	
	message := fmt.Sprintf(
		"🍎 認定整備品 入荷通知\n\n📱 %s\n� %s",
		p.ProductName,
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