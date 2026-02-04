package models

type ShopifyProductSearchResponse struct {
	Offers       []ShopifyOffer `json:"offers"`
	Instructions string         `json:"instructions"`
}

type ShopifyOffer struct {
	ID                string                     `json:"id"`
	Title             string                     `json:"title"`
	Description       string                     `json:"description"`
	Images            []ShopifyOfferImage        `json:"images"`           // legacy image shape
	Media             []ShopifyImage             `json:"media"`            // v2 API media
	Options           []ShopifyOption            `json:"options"`
	PriceRange        ShopifyPriceRange          `json:"priceRange"`
	Products          []ShopifyProduct           `json:"products"`         // v1 API products
	Variants          []ShopifyVariant           `json:"variants"`         // v2 API variants
	AvailableForSale  bool                       `json:"availableForSale"`
	Rating            *ShopifyRating             `json:"rating,omitempty"`
	UniqueSellingPoint string                    `json:"uniqueSellingPoint"`
	TopFeatures       []string                   `json:"topFeatures"`
	TechSpecs         []string                   `json:"techSpecs"`
	SharedAttributes  []ShopifySharedAttribute   `json:"sharedAttributes"`
	URL               string                     `json:"url"`              // legacy URL
	LookupURL         string                     `json:"lookupUrl"`        // v2 API URL
}

type ShopifyOfferImage struct {
	URL     string              `json:"url"`
	AltText string              `json:"altText"`
	Product *ShopifyImageProduct `json:"product,omitempty"`
}

type ShopifyImageProduct struct {
	ID             string               `json:"id"`
	Title          string               `json:"title"`
	OnlineStoreURL string               `json:"onlineStoreUrl"`
	Shop           ShopifyShopSummary   `json:"shop"`
}

type ShopifyShopSummary struct {
	Name           string `json:"name"`
	OnlineStoreURL string `json:"onlineStoreUrl"`
}

type ShopifyOption struct {
	Name   string                     `json:"name"`
	Values []ShopifyOptionValueDetail `json:"values"`
}

type ShopifyOptionValueDetail struct {
	Value            string `json:"value"`
	AvailableForSale bool   `json:"availableForSale"`
	Exists           bool   `json:"exists"`
}

type ShopifyPriceRange struct {
	Min ShopifyMoney `json:"min"`
	Max ShopifyMoney `json:"max"`
}

type ShopifyMoney struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

type ShopifyProduct struct {
	ID                    string                     `json:"id"`
	Title                 string                     `json:"title"`
	CheckoutURL           string                     `json:"checkoutUrl"`
	Description           string                     `json:"description"`
	FeaturedImage         ShopifyImage               `json:"featuredImage"`
	OnlineStoreURL        string                     `json:"onlineStoreUrl"`
	Price                 ShopifyMoney               `json:"price"`
	Rating                *ShopifyRating             `json:"rating,omitempty"`
	AvailableForSale      bool                       `json:"availableForSale"`
	Shop                  ShopifyShop                `json:"shop"`
	SelectedProductVariant ShopifyProductVariant     `json:"selectedProductVariant"`
	Secondhand            bool                       `json:"secondhand"`
	RequiresSellingPlan   bool                       `json:"requiresSellingPlan"`
}

func (product *ShopifyProduct) Standardise() Product {
	p := Product{
		Id:          product.ID,
		Title:       product.Title,
		Description: product.Description,
	}

	// Build a single variant from the selected product variant, if present.
	if product.SelectedProductVariant.ID != "" {
		v := Variant{
			Id:    product.SelectedProductVariant.ID,
			Title: product.Title,
			// For v1 products, OnlineStoreURL is the product page with the variant selected.
			Url: product.OnlineStoreURL,
		}

		// Variant image (fallback to product featured image).
		if product.SelectedProductVariant.Image.URL != "" {
			v.Image = ImageWithUrl{
				Url: product.SelectedProductVariant.Image.URL,
				Alt: product.SelectedProductVariant.Image.AltText,
			}
		} else {
			v.Image = ImageWithUrl{
				Url: product.FeaturedImage.URL,
				Alt: product.FeaturedImage.AltText,
			}
		}

		// Variant options.
		for _, opt := range product.SelectedProductVariant.Options {
			v.Options = append(v.Options, VariantOption{
				Name:  opt.Name,
				Value: opt.Value,
			})
		}

		// Variant price.
		v.Price = Money{
			Amount:   product.SelectedProductVariant.Price.Amount,
			Currency: product.SelectedProductVariant.Price.Currency,
		}

		v.AvailableForSale = product.SelectedProductVariant.AvailableForSale

		p.Variants = append(p.Variants, v)
	}

	return p
}

type ShopifyImage struct {
	URL     string `json:"url"`
	AltText string `json:"altText"`
}

type ShopifyRating struct {
	Value float64 `json:"value"`
	Count int     `json:"count"`
}

type ShopifyShop struct {
	Name            string                 `json:"name"`
	PaymentSettings ShopifyPaymentSettings `json:"paymentSettings"`
	OnlineStoreURL  string                 `json:"onlineStoreUrl"`
	PrivacyPolicy   *ShopifyPolicy         `json:"privacyPolicy,omitempty"`
	RefundPolicy    *ShopifyPolicy         `json:"refundPolicy,omitempty"`
	TermsOfService  *ShopifyPolicy         `json:"termsOfService,omitempty"`
	ShippingPolicy  *ShopifyPolicy         `json:"shippingPolicy,omitempty"`
	ID              string                 `json:"id"`
}

type ShopifyPaymentSettings struct {
	SupportedDigitalWallets []string `json:"supportedDigitalWallets"`
	AcceptedCardBrands      []string `json:"acceptedCardBrands"`
}

type ShopifyPolicy struct {
	URL string `json:"url"`
}

type ShopifyProductVariant struct {
	ID              string               `json:"id"`
	AvailableForSale bool                `json:"availableForSale"`
	Options         []ShopifyVariantOption `json:"options"`
	Price           ShopifyMoney         `json:"price"`
	Image           ShopifyImage         `json:"image"`
}

// ShopifyVariant represents the v2 "variants" entries under an offer.
type ShopifyVariant struct {
	ID              string               `json:"id"`
	ProductID       string               `json:"productId"`
	DisplayName     string               `json:"displayName"`
	AvailableForSale bool                `json:"availableForSale"`
	Price           ShopifyMoney         `json:"price"`
	Media           []ShopifyImage       `json:"media"`
	Options         []ShopifyVariantOption `json:"options"`
	Shop            ShopifyShopSummary   `json:"shop"`
	VariantURL      string               `json:"variantUrl"`
	CheckoutURL     string               `json:"checkoutUrl"`
	Secondhand      bool                 `json:"secondhand"`
	LookupURL       string               `json:"lookupUrl"`
}

type ShopifyVariantOption struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ShopifySharedAttribute struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// Standardise turns a high-level offer into a generic Product. This is useful
// when the API returns offers + variants instead of fully shaped products.
func (offer *ShopifyOffer) Standardise() Product {
	p := Product{
		Id:          offer.ID,
		Title:       offer.Title,
		Description: offer.Description,
	}

	// Choose a featured image: prefer offer-level media, then variant media,
	// then legacy product featured image if present.
	var img ImageWithUrl
	if len(offer.Media) > 0 {
		img.Url = offer.Media[0].URL
		img.Alt = offer.Media[0].AltText
	} else if len(offer.Variants) > 0 && len(offer.Variants[0].Media) > 0 {
		img.Url = offer.Variants[0].Media[0].URL
		img.Alt = offer.Variants[0].Media[0].AltText
	} else if len(offer.Products) > 0 {
		img.Url = offer.Products[0].FeaturedImage.URL
		img.Alt = offer.Products[0].FeaturedImage.AltText
	}

	// Build per-variant data with URLs/images/options.
	for _, v := range offer.Variants {
		varUrl := ""
		if v.VariantURL != "" {
			varUrl = v.VariantURL
		} else if v.LookupURL != "" {
			varUrl = v.LookupURL
		}

		varImg := img
		if len(v.Media) > 0 {
			varImg = ImageWithUrl{
				Url: v.Media[0].URL,
				Alt: v.Media[0].AltText,
			}
		}

		variant := Variant{
			Id:              v.ID,
			Title:           v.DisplayName,
			Url:             varUrl,
			Image:           varImg,
			Price: Money{
				Amount:   v.Price.Amount,
				Currency: v.Price.Currency,
			},
			AvailableForSale: v.AvailableForSale,
		}

		for _, opt := range v.Options {
			variant.Options = append(variant.Options, VariantOption{
				Name:  opt.Name,
				Value: opt.Value,
			})
		}

		p.Variants = append(p.Variants, variant)
	}

	return p
}