// gen-dataset generates a synthetic dataset for online-boutique workflows.
//
// Products are the real catalog (9 items, hardcoded from productcatalogservice).
// Users are generated with plausible fake addresses and Luhn-valid card numbers.
//
// Usage:
//
//	# NDJSON for zeus upload API
//	go run ./cmd/gen-dataset -users 100 | curl -X POST \
//	  http://zeus:8080/api/v1/datasets/boutique/upload \
//	  -H 'Content-Type: application/x-ndjson' --data-binary @-
//
//	# Plain JSON for local k6 data.json
//	go run ./cmd/gen-dataset -users 50 -format json > k6/flows/online-boutique/data.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
)

// The 9 products from online-boutique's productcatalogservice.
var products = []map[string]any{
	{"id": "OLJCESPC7Z", "name": "sunglasses", "price": 19.99},
	{"id": "66VCHSJNUP", "name": "tank top", "price": 18.99},
	{"id": "1YMWWN1N4O", "name": "watch", "price": 109.99},
	{"id": "L9ECAV7KIM", "name": "loafers", "price": 89.99},
	{"id": "2ZYFJ3GM2N", "name": "hairdryer", "price": 24.99},
	{"id": "0PUK6V6EV0", "name": "candles", "price": 3.99},
	{"id": "LS4PSXUNUM", "name": "salt & pepper", "price": 18.99},
	{"id": "9SIQT8TOJO", "name": "bamboo glass jar", "price": 5.49},
	{"id": "6E92ZMYYFZ", "name": "mug", "price": 8.99},
}

var currencies = []map[string]any{
	{"code": "USD", "symbol": "$"},
	{"code": "EUR", "symbol": "\u20ac"},
	{"code": "JPY", "symbol": "\u00a5"},
	{"code": "CAD", "symbol": "CA$"},
	{"code": "GBP", "symbol": "\u00a3"},
}

// Address pool — small set of real-ish addresses across regions.
var addresses = []struct {
	Street  string
	City    string
	State   string
	Zip     string
	Country string
}{
	{"1600 Amphitheatre Parkway", "Mountain View", "CA", "94043", "United States"},
	{"350 Fifth Avenue", "New York", "NY", "10118", "United States"},
	{"1 Infinite Loop", "Cupertino", "CA", "95014", "United States"},
	{"233 S Wacker Dr", "Chicago", "IL", "60606", "United States"},
	{"1000 4th Ave", "Seattle", "WA", "98104", "United States"},
	{"100 Universal City Plaza", "Los Angeles", "CA", "91608", "United States"},
	{"200 Clarendon St", "Boston", "MA", "02116", "United States"},
	{"600 Congress Ave", "Austin", "TX", "78701", "United States"},
	{"1 Hacker Way", "Menlo Park", "CA", "94025", "United States"},
	{"410 Terry Ave N", "Seattle", "WA", "98109", "United States"},
}

func main() {
	users := flag.Int("users", 50, "number of synthetic users to generate")
	format := flag.String("format", "ndjson", "output format: ndjson (zeus upload) or json (k6 data file)")
	flag.Parse()

	if *users < 1 {
		fmt.Fprintln(os.Stderr, "gen-dataset: -users must be >= 1")
		os.Exit(1)
	}

	userRows := make([]map[string]any, *users)
	for i := range *users {
		userRows[i] = generateUser(i)
	}

	switch *format {
	case "ndjson":
		writeNDJSON(userRows)
	case "json":
		writeJSON(userRows)
	default:
		fmt.Fprintf(os.Stderr, "gen-dataset: unknown format %q (use ndjson or json)\n", *format)
		os.Exit(1)
	}
}

func writeNDJSON(users []map[string]any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	_ = enc.Encode(map[string]any{"pool": "products", "rows": products})
	_ = enc.Encode(map[string]any{"pool": "currencies", "rows": currencies})

	// Batch users in chunks of 100 to keep line sizes reasonable.
	for i := 0; i < len(users); i += 100 {
		end := min(i+100, len(users))
		_ = enc.Encode(map[string]any{"pool": "users", "rows": users[i:end]})
	}
}

func writeJSON(users []map[string]any) {
	dataset := map[string]any{
		"products":   products,
		"currencies": currencies,
		"users":      users,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(dataset)
}

func generateUser(i int) map[string]any {
	addr := addresses[i%len(addresses)]
	return map[string]any{
		"email":                        fmt.Sprintf("user-%d@loadgen.local", i+1),
		"street_address":               addr.Street,
		"zip_code":                     addr.Zip,
		"city":                         addr.City,
		"state":                        addr.State,
		"country":                      addr.Country,
		"credit_card_number":           luhnCard(i),
		"credit_card_expiration_month": rand.IntN(12) + 1,
		"credit_card_expiration_year":  2027 + rand.IntN(4),
		"credit_card_cvv":             100 + rand.IntN(900),
	}
}

// luhnCard generates a Luhn-valid 16-digit card number with a 4532 BIN prefix.
// Not a real card — just structurally valid for services that check the checksum.
func luhnCard(seed int) string {
	// Fixed BIN prefix + seed-derived middle digits.
	digits := [16]int{4, 5, 3, 2}
	for i := 4; i < 15; i++ {
		digits[i] = (seed*7 + i*3) % 10
	}

	// Luhn checksum for the last digit.
	sum := 0
	for i := 14; i >= 0; i-- {
		d := digits[i]
		if (15-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	digits[15] = (10 - sum%10) % 10

	return fmt.Sprintf("%d%d%d%d-%d%d%d%d-%d%d%d%d-%d%d%d%d",
		digits[0], digits[1], digits[2], digits[3],
		digits[4], digits[5], digits[6], digits[7],
		digits[8], digits[9], digits[10], digits[11],
		digits[12], digits[13], digits[14], digits[15])
}
