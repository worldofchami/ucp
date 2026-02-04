package models

type ImageWithUrl struct {
	Url 		string 		`json:"url"`
	Alt 		string 		`json:"alt"`
}

// Money represents a monetary value in a specific currency.
// The amount is kept in the same units as provided by the
// underlying platform (for Shopify this is currently minor
// units such as cents).
type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type Product struct{
	Id 				string 		`json:"id"`
	Title 			string 		`json:"title"`
	Description 	string 		`json:"description"`
	// Variants contains the variant-level URLs/images/options for this product.
	Variants		[]Variant	`json:"variants,omitempty"`
}

// Variant represents a purchasable variant of a product, with its own URL and image.
type Variant struct {
	Id              string          `json:"id"`
	Title           string          `json:"title"`
	Url             string          `json:"url,omitempty"`
	Image           ImageWithUrl    `json:"image"`
	// Price is the per-variant price, when available.
	Price           Money           `json:"price,omitempty"`
	Options         []VariantOption `json:"options,omitempty"`
	AvailableForSale bool           `json:"available_for_sale"`
}

type VariantOption struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (product *Product) Standardise() (Product, error) {
	return *product, nil
}

type PlatformProduct interface {
	Standardise() 	Product
}

type JSONRPCResponse struct {
	JSONRPC 		string 		`json:"jsonrpc"`
	Id      		int 		`json:"id"`
	Result  		any 		`json:"result"`
}