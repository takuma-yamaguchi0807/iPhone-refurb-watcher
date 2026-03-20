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

// JSON-LD 形式（HTMLから抽出される構造）
type JsonLDProduct struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// 監視対象のフィルター条件
var (
	targetModels    = []string{"iPhone 15", "iPhone 16"}
	targetStorages  = []string{"128GB", "256GB", "512GB"}
	appleRefurbURL  = "https://www.apple.com/jp/shop/refurbished/iphone"
)

func main() {
	log.Printf("=== iPhone 整備品チェッカー 開始 ===")
	
	lineToken := os.Getenv("LINE_CHANNEL_ACCESS_TOKEN")
	lineUserID := os.Getenv("LINE_USER_ID")
	seenFile := os.Getenv("SEEN_FILE")
	if seenFile == "" {
		seenFile = "seen.json"
	}

	log.Printf("設定確認:")
	log.Printf("  LINE トークン: %s (セット: %v)", maskToken(lineToken), lineToken != "")
	log.Printf("  LINE ユーザーID: %s", lineUserID)
	log.Printf("  seen.json パス: %s", seenFile)

	if lineToken == "" || lineUserID == "" {
		log.Fatal("LINE_CHANNEL_ACCESS_TOKEN と LINE_USER_ID を環境変数に設定してください")
	}

	// 既通知済みの商品IDを読み込む
	seen := loadSeen(seenFile)
	log.Printf("既通知リスト読み込み: %d件", len(seen))

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
	log.Printf("=== 完了 ===")
}

func fetchProducts() ([]Product, error) {
	log.Printf("Apple API からデータ取得開始: %s", appleRefurbURL)
	
	client := &http.Client{}
	req, err := http.NewRequest("GET", appleRefurbURL, nil)
	if err != nil {
		log.Printf("リクエスト作成失敗: %v", err)
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "ja-JP,ja;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("HTTP リクエスト失敗: %v", err)
		return nil, err
	}
	defer resp.Body.Close()

	log.Printf("HTTP レスポンス ステータス: %d", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTPステータス: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("レスポンス読み込み失敗: %v", err)
		return nil, err
	}

	log.Printf("レスポンス サイズ: %d bytes", len(body))

	// HTMLから JSON-LD を抽出 → Product に変換
	products, err := extractProducts(string(body))
	if err != nil {
		log.Printf("商品抽出失敗: %v", err)
		return nil, err
	}

	log.Printf("商品抽出成功: %d件", len(products))
	for i, p := range products {
		log.Printf("  [%d] %s", i+1, p.ProductName)
	}

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
	idx := 0
	jsonLDCount := 0
	
	doc.Find("script[type='application/ld+json']").Each(func(i int, s *goquery.Selection) {
		jsonLDCount++
		jsonText := s.Text()
		log.Printf("JSON-LD [%d] 発見 (サイズ: %d bytes)", jsonLDCount, len(jsonText))

		var j JsonLDProduct
		if err := json.Unmarshal([]byte(jsonText), &j); err != nil {
			log.Printf("  → JSON パース失敗: %v", err)
			return
		}

		log.Printf("  → name: %s", j.Name)
		log.Printf("  → url: %s", j.URL)
		
		// name と url があれば Product に追加
		if j.Name != "" && j.URL != "" {
			products = append(products, Product{
				PartNumber:  fmt.Sprintf("product_%d", idx),
				ProductName: j.Name,
				Price: struct {
					CurrentPrice float64 `json:"currentPrice"`
				}{CurrentPrice: 0},
				URL: j.URL,
			})
			log.Printf("  → Product に追加 [ID: product_%d]", idx)
			idx++
		} else {
			log.Printf("  → スキップ (name や url が空)")
		}
	})

	log.Printf("合計 JSON-LD タグ: %d個、Product 追加: %d件", jsonLDCount, len(products))

	if len(products) == 0 {
		return nil, fmt.Errorf("商品が見つかりません (JSON-LDタグ %d個検出)", jsonLDCount)
	}

	return products, nil
}



func filterProducts(products []Product) []Product {
	var matched []Product
	modelRe := buildRegexp(targetModels)
	storageRe := buildRegexp(targetStorages)

	log.Printf("フィルタリング開始")
	log.Printf("  対象モデル: %v", targetModels)
	log.Printf("  対象容量: %v", targetStorages)

	for _, p := range products {
		name := p.ProductName
		modelMatch := modelRe.MatchString(name)
		storageMatch := storageRe.MatchString(name)
		
		log.Printf("  [%s]", name)
		log.Printf("    モデル: %v, 容量: %v", modelMatch, storageMatch)
		
		if modelMatch && storageMatch {
			matched = append(matched, p)
			log.Printf("    → 条件に一致 ✓")
		} else {
			log.Printf("    → 不一致 ✗")
		}
	}

	log.Printf("フィルタリング完了: %d件 → %d件", len(products), len(matched))
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

// トークンをマスク表示（ログ出力用）
func maskToken(token string) string {
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}