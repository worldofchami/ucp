package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"
	"github.com/nlpodyssey/openai-agents-go/agents"
	"github.com/nlpodyssey/openai-agents-go/tracing"
	"github.com/twilio/twilio-go"
	openapi "github.com/twilio/twilio-go/rest/api/v2010"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Message represents a chat message
type Message struct {
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           string    `json:"role"` // "user" or "assistant"
	Content        string    `json:"content"`
	Timestamp      time.Time `json:"timestamp"`
	PhoneNumber    string    `json:"phone_number,omitempty"` // For Twilio integration
}

// User represents a user in the system
type User struct {
	ID              int64     `json:"id"`
	PhoneNumber     string    `json:"phone_number"`
	Name            string    `json:"name"`
	Email           string    `json:"email"`
	StreetAddress   string    `json:"street_address"`
	AddressLocality string    `json:"address_locality"`
	AddressRegion   string    `json:"address_region"`
	PostalCode      string    `json:"postal_code"`
	AddressCountry  string    `json:"address_country"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MessageHistory stores messages using GORM with raw SQL queries
type MessageHistory struct {
	db      *gorm.DB
	mu      sync.RWMutex
	maxSize int
}

func NewMessageHistory(dbPath string, maxSize int) (*MessageHistory, error) {
	// Open database with GORM
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Execute schema from SQL file
	schemaSQL := `
	CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		phone_number TEXT UNIQUE NOT NULL,
		name TEXT,
		email TEXT,
		street_address TEXT,
		address_locality TEXT,
		address_region TEXT,
		postal_code TEXT,
		address_country TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS conversations (
		id TEXT PRIMARY KEY,
		user_id TEXT REFERENCES users(id),
		phone_number TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL,
		role TEXT NOT NULL CHECK(role IN ('user', 'assistant')),
		content TEXT NOT NULL,
		phone_number TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (conversation_id) REFERENCES conversations(id)
	);

	CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_users_phone ON users(phone_number);
	`

	if err := db.Exec(schemaSQL).Error; err != nil {
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	return &MessageHistory{
		db:      db,
		maxSize: maxSize,
	}, nil
}

func (mh *MessageHistory) Close() error {
	sqlDB, err := mh.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (mh *MessageHistory) AddMessage(conversationID string, msg Message) error {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	// Raw SQL: Insert or update conversation using SQLite UPSERT syntax
	upsertConversation := `
		INSERT INTO conversations (id, phone_number, created_at, updated_at) 
		VALUES (?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(id) DO UPDATE SET updated_at = datetime('now')`

	if err := mh.db.Exec(upsertConversation, conversationID, msg.PhoneNumber).Error; err != nil {
		return fmt.Errorf("failed to upsert conversation: %w", err)
	}

	// Raw SQL: Insert message
	insertMessage := `
		INSERT INTO messages (conversation_id, role, content, phone_number, created_at) 
		VALUES (?, ?, ?, ?, datetime('now'))`

	if err := mh.db.Exec(insertMessage, conversationID, msg.Role, msg.Content, msg.PhoneNumber).Error; err != nil {
		return fmt.Errorf("failed to insert message: %w", err)
	}

	// Raw SQL: Keep only last maxSize messages for this conversation
	trimMessages := `
		DELETE FROM messages WHERE conversation_id = ? AND id NOT IN (
			SELECT id FROM messages 
			WHERE conversation_id = ? 
			ORDER BY created_at DESC 
			LIMIT ?
		)`

	if err := mh.db.Exec(trimMessages, conversationID, conversationID, mh.maxSize).Error; err != nil {
		return fmt.Errorf("failed to trim messages: %w", err)
	}

	return nil
}

func (mh *MessageHistory) GetHistory(conversationID string) ([]Message, error) {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	// Raw SQL: Select messages
	query := `
		SELECT id, conversation_id, role, content, phone_number, created_at 
		FROM messages 
		WHERE conversation_id = ? 
		ORDER BY created_at ASC`

	rows, err := mh.db.Raw(query, conversationID).Rows()
	if err != nil {
		return nil, fmt.Errorf("failed to query messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var createdAt string
		err := rows.Scan(&msg.ID, &msg.ConversationID, &msg.Role, &msg.Content, &msg.PhoneNumber, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan message: %w", err)
		}
		msg.Timestamp, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

func (mh *MessageHistory) GetHistoryAsContext(conversationID string) (string, error) {
	msgs, err := mh.GetHistory(conversationID)
	if err != nil {
		return "", err
	}

	if len(msgs) == 0 {
		return "", nil
	}

	var context strings.Builder
	context.WriteString("\n\n=== Recent Conversation History ===\n")
	for _, msg := range msgs {
		role := "User"
		if msg.Role == "assistant" {
			role = "Assistant"
		}
		context.WriteString(fmt.Sprintf("%s: %s\n", role, msg.Content))
	}
	context.WriteString("=== End of History ===\n")
	return context.String(), nil
}

// GetOrCreateUser gets an existing user by phone number or creates a new one
func (mh *MessageHistory) GetOrCreateUser(phoneNumber string) (*User, error) {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	// Try to get existing user
	var user User
	query := `SELECT id, phone_number, name, email, street_address, address_locality, address_region, postal_code, address_country, created_at, updated_at FROM users WHERE phone_number = ?`
	row := mh.db.Raw(query, phoneNumber).Row()
	err := row.Scan(&user.ID, &user.PhoneNumber, &user.Name, &user.Email, &user.StreetAddress, &user.AddressLocality, &user.AddressRegion, &user.PostalCode, &user.AddressCountry, &user.CreatedAt, &user.UpdatedAt)

	if err == nil {
		return &user, nil
	}

	// Create new user if not found
	userID := fmt.Sprintf("user_%d", time.Now().UnixNano())
	insertQuery := `
		INSERT INTO users (id, phone_number, name, email, street_address, address_locality, address_region, postal_code, address_country, created_at, updated_at)
		VALUES (?, ?, NULL, NULL, NULL, NULL, NULL, NULL, NULL, datetime('now'), datetime('now'))`

	if err := mh.db.Exec(insertQuery, userID, phoneNumber).Error; err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	// Return the newly created user
	user = User{
		ID:          0, // Will be populated on next query
		PhoneNumber: phoneNumber,
		Name:        "",
	}
	return &user, nil
}

// GetUserByPhone gets a user by phone number
func (mh *MessageHistory) GetUserByPhone(phoneNumber string) (*User, error) {
	mh.mu.RLock()
	defer mh.mu.RUnlock()

	var user User
	query := `SELECT id, phone_number, name, email, street_address, address_locality, address_region, postal_code, address_country, created_at, updated_at FROM users WHERE phone_number = ?`
	row := mh.db.Raw(query, phoneNumber).Row()
	err := row.Scan(&user.ID, &user.PhoneNumber, &user.Name, &user.Email, &user.StreetAddress, &user.AddressLocality, &user.AddressRegion, &user.PostalCode, &user.AddressCountry, &user.CreatedAt, &user.UpdatedAt)

	if err != nil {
		return nil, err
	}

	return &user, nil
}

// UpdateUserField updates a specific user field
func (mh *MessageHistory) UpdateUserField(phoneNumber, field, value string) error {
	mh.mu.Lock()
	defer mh.mu.Unlock()

	// Validate field name to prevent SQL injection
	validFields := map[string]bool{
		"name":             true,
		"email":            true,
		"street_address":   true,
		"address_locality": true,
		"address_region":   true,
		"postal_code":      true,
		"address_country":  true,
	}

	if !validFields[field] {
		return fmt.Errorf("invalid field name: %s", field)
	}

	query := fmt.Sprintf(`
		UPDATE users 
		SET %s = ?, updated_at = datetime('now')
		WHERE phone_number = ?`, field)

	if err := mh.db.Exec(query, value, phoneNumber).Error; err != nil {
		return fmt.Errorf("failed to update user field %s: %w", field, err)
	}

	return nil
}

// TwilioClient handles SMS sending via Twilio
type TwilioClient struct {
	client      *twilio.RestClient
	phoneNumber string
	configured  bool
}

// NewTwilioClient creates a new Twilio client from environment variables
func NewTwilioClient() *TwilioClient {
	accountSID := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")
	phoneNumber := os.Getenv("TWILIO_PHONE_NUMBER")

	if accountSID == "" || authToken == "" || phoneNumber == "" {
		return &TwilioClient{
			configured: false,
		}
	}

	client := twilio.NewRestClientWithParams(twilio.ClientParams{
		Username: accountSID,
		Password: authToken,
	})

	return &TwilioClient{
		client:      client,
		phoneNumber: phoneNumber,
		configured:  true,
	}
}

// IsConfigured returns true if the client is properly configured
func (t *TwilioClient) IsConfigured() bool {
	return t.configured
}

// GetPhoneNumber returns the configured Twilio phone number
func (t *TwilioClient) GetPhoneNumber() string {
	return t.phoneNumber
}

// SendSMS sends an SMS or MMS message to the specified phone number
func (t *TwilioClient) SendSMS(toPhoneNumber, message, imageURL string) error {
	if !t.configured {
		return fmt.Errorf("twilio client not configured")
	}

	// Format phone numbers - ensure they have + prefix
	toPhoneNumber = formatPhoneNumber(toPhoneNumber)
	fromPhoneNumber := formatPhoneNumber(t.phoneNumber)

	// Truncate message if exceeds Twilio limit (1600 chars)
	const maxLength = 1600
	if len(message) > maxLength {
		fmt.Fprintf(os.Stderr, "[Twilio] Warning: Message exceeds %d characters, truncating...\n", maxLength)
		message = message[:maxLength-3] + "..."
	}

	if imageURL != "" {
		fmt.Fprintf(os.Stderr, "[Twilio] Sending MMS from %s to %s with image (length: %d)\n", fromPhoneNumber, toPhoneNumber, len(message))
	} else {
		fmt.Fprintf(os.Stderr, "[Twilio] Sending SMS from %s to %s (length: %d)\n", fromPhoneNumber, toPhoneNumber, len(message))
	}

	params := &openapi.CreateMessageParams{
		To:   &toPhoneNumber,
		From: &fromPhoneNumber,
		Body: &message,
	}

	// Add image if provided (MMS)
	if imageURL != "" {
		params.SetMediaUrl([]string{imageURL})
	}

	_, err := t.client.Api.CreateMessage(params)
	if err != nil {
		return fmt.Errorf("failed to send SMS: %w", err)
	}

	return nil
}

// formatPhoneNumber ensures phone number has proper E.164 format (+ prefix)
func formatPhoneNumber(phone string) string {
	phone = strings.TrimSpace(phone)
	// Remove any non-numeric characters except +
	var result strings.Builder
	for _, char := range phone {
		if char >= '0' && char <= '9' || char == '+' {
			result.WriteRune(char)
		}
	}
	phone = result.String()

	// Add + prefix if missing
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}

	if !strings.HasPrefix(phone, "whatsapp:") {
		phone = "whatsapp:" + phone
	}

	return phone
}

// chatRequest is the payload accepted by the /chat endpoint.
type chatRequest struct {
	Prompt string `json:"prompt"`
}

// chatResponse is the JSON shape returned by the /chat endpoint.
type chatResponse struct {
	Output string `json:"output"`
}

// ConversationContext holds context for a conversation including SMS capabilities
type ConversationContext struct {
	PhoneNumber    string
	UserName       string
	TwilioClient   *TwilioClient
	MessageHistory *MessageHistory
}

// SendSMSParams parameters for sending SMS/MMS
type SendSMSParams struct {
	Message  string `json:"message"`
	ImageURL string `json:"image_url,omitempty"`
}

// UpdateUserFieldParams parameters for updating user fields
type UpdateUserFieldParams struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// sendSMSTool creates a tool that allows the agent to send SMS/MMS messages
func sendSMSTool(ctx *ConversationContext) agents.FunctionTool {
	return agents.NewFunctionTool(
		"send_sms",
		"Send an SMS or MMS message to the user. Use this to send product information with images. Each product should be sent as a separate message. Include the product image URL when available for a richer experience.",
		func(_ context.Context, params SendSMSParams) (string, error) {
			if ctx.TwilioClient == nil || !ctx.TwilioClient.IsConfigured() {
				return "", fmt.Errorf("SMS not available - Twilio not configured")
			}

			if err := ctx.TwilioClient.SendSMS(ctx.PhoneNumber, params.Message, params.ImageURL); err != nil {
				return "", fmt.Errorf("failed to send SMS: %w", err)
			}

			// Store the sent message in history
			if ctx.MessageHistory != nil {
				content := params.Message
				if params.ImageURL != "" {
					content += " [Image: " + params.ImageURL + "]"
				}
				_ = ctx.MessageHistory.AddMessage(ctx.PhoneNumber, Message{
					Role:        "assistant",
					Content:     content,
					Timestamp:   time.Now(),
					PhoneNumber: ctx.PhoneNumber,
				})
			}

			return "SMS sent successfully", nil
		},
	)
}

// updateUserFieldTool creates a tool that allows the agent to update any user field
func updateUserFieldTool(ctx *ConversationContext) agents.FunctionTool {
	return agents.NewFunctionTool(
		"update_user_field",
		"Update any user information field. Valid fields are: name, email, street_address, address_locality, address_region, postal_code, address_country. Use this whenever the user provides personal information.",
		func(_ context.Context, params UpdateUserFieldParams) (string, error) {
			if ctx.MessageHistory == nil {
				return "", fmt.Errorf("message history not available")
			}

			if err := ctx.MessageHistory.UpdateUserField(ctx.PhoneNumber, params.Field, params.Value); err != nil {
				return "", fmt.Errorf("failed to update user field: %w", err)
			}

			// Update context if name was changed
			if params.Field == "name" {
				ctx.UserName = params.Value
				return fmt.Sprintf("Got it! Nice to meet you, %s! ðŸ˜Š", params.Value), nil
			}

			return fmt.Sprintf("Updated your %s. Thanks! ðŸ‘�", params.Field), nil
		},
	)
}

func main() {
	addr := getEnv("CHAT_SERVER_ADDR", ":8090")

	godotenv.Load()

	// Disable OpenAI tracing to prevent console spam
	tracing.SetTracingDisabled(true)

	// Initialize message history with SQLite (stores last 10 messages per conversation)
	dbPath := getEnv("CHAT_DB_PATH", "./chat_history.db")
	messageHistory, err := NewMessageHistory(dbPath, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize database: %v\n", err)
		os.Exit(1)
	}
	defer messageHistory.Close()

	// Initialize Twilio client
	twilioClient := NewTwilioClient()
	if twilioClient.IsConfigured() {
		fmt.Fprintf(os.Stderr, "Twilio client initialized\n")
	} else {
		fmt.Fprintf(os.Stderr, "Warning: Twilio not configured (set TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_PHONE_NUMBER)\n")
	}

	r := chi.NewRouter()

	// Simple health check.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Twilio webhook endpoint for receiving SMS messages
	r.Post("/twilio/webhook", func(w http.ResponseWriter, r *http.Request) {
		// Parse Twilio webhook payload
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form data", http.StatusBadRequest)
			return
		}

		from := r.FormValue("From")
		body := r.FormValue("Body")

		// Check if this is a reply to a specific message (WhatsApp reply-to feature)
		// Twilio sends the original message SID in various possible fields
		originalMessageSid := ""
		possibleFields := []string{
			"OriginalRepliedMessageSid", // Primary field for WhatsApp reply-to
			"RepliedMessageSid",         // Alternative field name
			"QuotedMessageSid",          // Another possible field
			"Context",                   // May contain reply context
			"MessageSid",                // Current message SID
		}

		for _, field := range possibleFields {
			if value := r.FormValue(field); value != "" {
				originalMessageSid = value
				fmt.Fprintf(os.Stderr, "[Twilio] Found reply-to field %s: %s\n", field, value)
				break
			}
		}

		// Log ALL form values for debugging
		fmt.Fprintf(os.Stderr, "[Twilio] All form values: %v\n", r.Form)

		if from == "" || body == "" {
			http.Error(w, "Missing From or Body", http.StatusBadRequest)
			return
		}

		// Format the from number for consistency
		fromFormatted := formatPhoneNumber(from)

		// Log reply-to information if present
		if originalMessageSid != "" {
			fmt.Fprintf(os.Stderr, "[Twilio] Received REPLY from %s to message %s: %s\n", fromFormatted, originalMessageSid, body)
		} else {
			fmt.Fprintf(os.Stderr, "[Twilio] Received message from %s (formatted: %s): %s\n", from, fromFormatted, body)
		}
		fmt.Fprintf(os.Stderr, "[Twilio] Configured Twilio phone number: %s\n", formatPhoneNumber(twilioClient.GetPhoneNumber()))

		// Store user message in history with reply-to context if present
		messageContent := body
		if originalMessageSid != "" {
			messageContent = fmt.Sprintf("[Reply to message %s]: %s", originalMessageSid, body)
		}

		if err := messageHistory.AddMessage(fromFormatted, Message{
			Role:        "user",
			Content:     messageContent,
			Timestamp:   time.Now(),
			PhoneNumber: fromFormatted,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "[Twilio] Failed to store user message: %v\n", err)
		}

		// Process the message with context
		// The agent will handle sending SMS messages for products using the send_sms tool
		response := processTwilioMessage(r.Context(), messageHistory, twilioClient, fromFormatted, body)

		// Store final response in history
		if response != "" {
			if err := messageHistory.AddMessage(fromFormatted, Message{
				Role:        "assistant",
				Content:     response,
				Timestamp:   time.Now(),
				PhoneNumber: fromFormatted,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "[Twilio] Failed to store assistant message: %v\n", err)
			}

			// Send final response via Twilio API
			go func() {
				if err := twilioClient.SendSMS(fromFormatted, response, ""); err != nil {
					fmt.Fprintf(os.Stderr, "[Twilio] Failed to send SMS: %v\n", err)
				} else {
					fmt.Fprintf(os.Stderr, "[Twilio] SMS sent successfully to %s\n", fromFormatted)
				}
			}()
		}

		// Return 200 OK to acknowledge receipt
		w.WriteHeader(http.StatusOK)
	})

	// Main chat endpoint with context support.
	r.Post("/chat", func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		req.Prompt = strings.TrimSpace(req.Prompt)
		if req.Prompt == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}

		// Get session ID from header (for conversation tracking)
		sessionID := r.Header.Get("X-Session-ID")
		if sessionID == "" {
			sessionID = "default"
		}

		// Store user message
		messageHistory.AddMessage(sessionID, Message{
			Role:      "user",
			Content:   req.Prompt,
			Timestamp: time.Now(),
		})

		// Get or create user for this session
		user, err := messageHistory.GetOrCreateUser(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Chat] Failed to get/create user: %v\n", err)
			user = &User{PhoneNumber: sessionID, Name: ""}
		}

		// Get conversation context
		conversationContext, err := messageHistory.GetHistoryAsContext(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Chat] Failed to get history: %v\n", err)
			conversationContext = ""
		}

		// Build prompt with context
		promptWithContext := req.Prompt
		if conversationContext != "" {
			promptWithContext = req.Prompt + conversationContext
		}

		// Build a shopping-assistant agent on each request.
		agent := agents.New("ShoppingAssistant").
			WithInstructions(baseInstructions(*user)).
			WithModel("gpt-4o").
			WithTools(
				mcpDiscoverStoreTool(),
				mcpSearchProductsTool(),
				mcpSearchShopCatalogTool(),
				mcpGetCartTool(),
				mcpUpdateCartTool(),
				mcpCreateCheckoutTool(),
				mcpSearchShopPoliciesTool(),
			)

		// Run the agent against the user's prompt with context.
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()

		result, err := agents.Run(ctx, agent, promptWithContext)
		if err != nil {
			http.Error(w, fmt.Sprintf("agent error: %v", err), http.StatusInternalServerError)
			return
		}

		response := result.FinalOutput.(string)

		// Store assistant response
		messageHistory.AddMessage(sessionID, Message{
			Role:      "assistant",
			Content:   response,
			Timestamp: time.Now(),
		})

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(chatResponse{Output: response}); err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
			return
		}
	})

	fmt.Fprintf(os.Stderr, "chat server listening on %s\n", addr)
	fmt.Fprintf(os.Stderr, "Twilio webhook: http://localhost%s/twilio/webhook\n", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		fmt.Fprintf(os.Stderr, "chat server error: %v\n", err)
		os.Exit(1)
	}
}

// processTwilioMessage processes a message from Twilio with context
func processTwilioMessage(ctx context.Context, history *MessageHistory, twilioClient *TwilioClient, phoneNumber, message string) string {
	// Get or create user
	user, err := history.GetOrCreateUser(phoneNumber)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Twilio] Failed to get/create user: %v\n", err)
		// Continue anyway, just without user context
		user = &User{PhoneNumber: phoneNumber, Name: ""}
	}

	// Create conversation context
	convCtx := &ConversationContext{
		PhoneNumber:    phoneNumber,
		UserName:       user.Name,
		TwilioClient:   twilioClient,
		MessageHistory: history,
	}

	// Get conversation history for context
	conversationContext, err := history.GetHistoryAsContext(phoneNumber)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Twilio] Failed to get history: %v\n", err)
		conversationContext = ""
	}

	// Build prompt with context
	promptWithContext := message
	if conversationContext != "" {
		promptWithContext = message + conversationContext
	}

	// Build a shopping-assistant agent with SMS capability
	agent := agents.New("ShoppingAssistant").
		WithInstructions(baseInstructions(*user)).
		WithModel("gpt-4o").
		WithTools(
			mcpDiscoverStoreTool(),
			mcpSearchProductsTool(),
			mcpSearchShopCatalogTool(),
			mcpGetCartTool(),
			mcpUpdateCartTool(),
			mcpCreateCheckoutTool(),
			mcpSearchShopPoliciesTool(),
			sendSMSTool(convCtx),
			updateUserFieldTool(convCtx),
		)

	// Run the agent
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := agents.Run(ctx, agent, promptWithContext)
	if err != nil {
		return fmt.Sprintf("Sorry, I encountered an error: %v", err)
	}

	return result.FinalOutput.(string)
}

// baseInstructions provides the system prompt for the shopping assistant.
func baseInstructions(user User) string {
	// Build user context based on what we know about the user
	userContext := ""
	if user.Name != "" {
		userContext += fmt.Sprintf("The user's name is %s. ", user.Name)
	}
	if user.StreetAddress != "" {
		userContext += "User has a saved shipping address. "
	}

	// Build address status for the agent
	addressStatus := ""
	if user.Name == "" {
		addressStatus += "- Name: NOT SET (ask for it!)\n"
	} else {
		addressStatus += fmt.Sprintf("- Name: %s ✓\n", user.Name)
	}
	if user.StreetAddress == "" {
		addressStatus += "- Street Address: NOT SET (ask for it!)\n"
	} else {
		addressStatus += "- Street Address: ✓\n"
	}
	if user.AddressLocality == "" {
		addressStatus += "- City: NOT SET\n"
	} else {
		addressStatus += fmt.Sprintf("- City: %s ✓\n", user.AddressLocality)
	}
	if user.PostalCode == "" {
		addressStatus += "- Postal Code: NOT SET\n"
	} else {
		addressStatus += fmt.Sprintf("- Postal Code: %s ✓\n", user.PostalCode)
	}
	if user.AddressCountry == "" {
		addressStatus += "- Country: NOT SET\n"
	} else {
		addressStatus += fmt.Sprintf("- Country: %s ✓\n", user.AddressCountry)
	}

	return strings.TrimSpace(fmt.Sprintf(`
You are a **friendly personal shopper** - think of yourself as a helpful, enthusiastic shopping buddy!

%s

USER INFORMATION STATUS:
%s

CRITICAL - USER INFO COLLECTION:
Before processing any shopping requests, collect the user's information at the RIGHT time:
1. FIRST: Get their name (if not set)
   - Ask warmly: "Hey there! 👋 I'm your personal shopping assistant! What's your name?"
   - Use update_user_field tool with field="name" to save it
   - ONLY proceed with shopping AFTER you know their name

2. WHEN THEY'RE READY TO ADD TO CART: Get their shipping address
   - ONLY ask for address when they're actually adding items to cart
   - Ask naturally: "To make checkout super quick, I'll need your shipping address. What's your street address? 📦"
   - Collect fields one by one as they provide them:
     * street_address (e.g., "123 Main Street")
     * address_locality (city)
     * address_region (state/province)
     * postal_code
     * address_country (country code like "US", "ZA", etc.)
   - Use update_user_field tool for each field

3. For browsing products, you only need their name - NOT the address

Your personality:
- Warm, friendly, and conversational - like texting a friend
- Use casual language, emojis when appropriate 😊
- Be enthusiastic about helping find the perfect item
- Ask follow-up questions to understand their style/preferences
- Give genuine recommendations with personality
- ALWAYS use the user's name in your responses once you know it

SMS PRODUCT MESSAGING WORKFLOW:
When you find products for a user, send each product as a separate, personalized SMS:
1. Search for products using search_products or search_shop_catalog tools
2. For EACH product found (maximum 3 products), send a separate SMS using the send_sms tool
3. Format each message conversationally with personality:
   - "Ooh, check this out! ✨ [Product Name] - $[Price] - [URL]"
   - "This would look amazing on you! 👗 [Product Name] - $[Price] - [URL]"
   - "Perfect for what you need! 🎯 [Product Name] - $[Price] - [URL]"
4. INCLUDE the product image URL in the MediaUrl parameter for MMS!
5. After sending all product messages, provide a brief, friendly summary

CHECKOUT PREFILL - CRITICAL:
When the user is ready to checkout:
- Use create_checkout tool with their stored address information
- Prefill ALL available fields: name, email (if you have it), and full shipping address
- The checkout should have minimal fields left for them to fill in
- Tell them: "I've prefilled your checkout with your saved info - just review and confirm! 🛒"

IMPORTANT RULES:
- Collect name FIRST before shopping
- ONLY ask for address when they're adding to cart (not when browsing)
- Send AT MOST 3 products total
- Each product gets its OWN SMS with personality
- ALWAYS include both product URL and image URL
- Use emojis to add warmth and personality
- Keep each SMS under 1600 characters
- Sound like a helpful friend, not a robot
- Always personalize by using their name
- Use update_user_field tool to save any info they provide
- Prefill checkout with ALL stored information

Guidelines:
- Get to know their preferences with friendly questions
- Use the available tools to discover stores and search for products
- Match your tone to their vibe - casual and fun!
- Make shopping feel exciting and personal
`, userContext, addressStatus))
}

// getEnv returns the value of the environment variable or a default.
func getEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// --- MCP integration helpers ------------------------------------------------

func getMCPServerURL() string {
	if url := os.Getenv("MCP_SERVER_URL"); url != "" {
		return url + "/rpc"
	}
	return "http://localhost:8080/rpc"
}

// mcpDiscoverStoreParams is the parameter shape for the discover-store tool.
type mcpDiscoverStoreParams struct {
	StoreURL string `json:"store_url"`
}

// mcpSearchProductsParams is the parameter shape for the search-products tool.
type mcpSearchProductsParams struct {
	Query   string `json:"query"`
	Context string `json:"context"`
}

// mcpToolsCallParams mirrors the JSON-RPC "tools/call" params expected by the
// MCP server implemented in cmd/mcp/main.go.
type mcpToolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// mcpRPCRequest is a minimal JSON-RPC 2.0 request payload.
type mcpRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// mcpContentItem mirrors the ucp.ContentItem shape well enough for our usage.
type mcpContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpRPCResult is the expected result field for tools/call as implemented in
// cmd/mcp/main.go (a map with a "content" array).
type mcpRPCResult struct {
	Content []mcpContentItem `json:"content"`
}

// mcpRPCError is a minimal JSON-RPC error shape.
type mcpRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// mcpRPCResponse captures just enough of the MCP JSON-RPC response.
type mcpRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  *mcpRPCResult   `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

// callMCP abstracts a single tools/call request to the MCP server and returns
// a concatenated text output built from all content items.
func callMCP(ctx context.Context, toolName string, args map[string]interface{}) (string, error) {
	reqBody := mcpRPCRequest{
		JSONRPC: "2.0",
		ID:      int(time.Now().UnixNano() / int64(time.Millisecond)),
		Method:  "tools/call",
		Params: mcpToolsCallParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal MCP request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, getMCPServerURL(), bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("build MCP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("call MCP server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("MCP server returned status %s", resp.Status)
	}

	var rpcResp mcpRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decode MCP response: %w", err)
	}

	if rpcResp.Error != nil {
		return "", fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return "", fmt.Errorf("MCP response missing result")
	}

	var builder strings.Builder
	for i, c := range rpcResp.Result.Content {
		if c.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		// Prefix each block slightly so the model can understand where it came from.
		fmt.Fprintf(&builder, "Result %d (%s):\n%s", i+1, c.Type, c.Text)
	}

	return builder.String(), nil
}

// mcpDiscoverStoreTool exposes the MCP ucp_discover tool to the agent.
func mcpDiscoverStoreTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"discover_store",
		"Discover a store's UCP configuration using the MCP server. Use this first when you know the store URL.",
		func(ctx context.Context, params mcpDiscoverStoreParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			out, err := callMCP(ctx, "ucp_discover", map[string]interface{}{
				"store_url": params.StoreURL,
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpSearchProductsTool exposes the MCP search_products tool to the agent.
func mcpSearchProductsTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"search_products",
		"Search for products using the MCP-backed Shopify discovery. Use natural language queries and provide helpful context.",
		func(ctx context.Context, params mcpSearchProductsParams) (string, error) {
			params.Query = strings.TrimSpace(params.Query)
			if params.Query == "" {
				return "", fmt.Errorf("query is required")
			}
			out, err := callMCP(ctx, "search_products", map[string]interface{}{
				"query":   params.Query,
				"context": strings.TrimSpace(params.Context),
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpSearchShopCatalogTool exposes the MCP search_shop_catalog tool to the agent.
type mcpSearchShopCatalogParams struct {
	StoreURL string `json:"store_url"`
	Query    string `json:"query"`
	Context  string `json:"context"`
}

func mcpSearchShopCatalogTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"search_shop_catalog",
		"Search a specific Shopify store's product catalog. Use this when the customer is browsing a specific store and you need to find products there.",
		func(ctx context.Context, params mcpSearchShopCatalogParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			params.Query = strings.TrimSpace(params.Query)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			if params.Query == "" {
				return "", fmt.Errorf("query is required")
			}
			out, err := callMCP(ctx, "search_shop_catalog", map[string]interface{}{
				"store_url": params.StoreURL,
				"query":     params.Query,
				"context":   strings.TrimSpace(params.Context),
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpGetCartTool exposes the MCP get_cart tool to the agent.
type mcpGetCartParams struct {
	StoreURL string `json:"store_url"`
	CartID   string `json:"cart_id"`
}

func mcpGetCartTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"get_cart",
		"Retrieve the current contents of a shopping cart, including items and checkout URL. Use this to show the customer what's in their cart.",
		func(ctx context.Context, params mcpGetCartParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			params.CartID = strings.TrimSpace(params.CartID)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			if params.CartID == "" {
				return "", fmt.Errorf("cart_id is required")
			}
			out, err := callMCP(ctx, "get_cart", map[string]interface{}{
				"store_url": params.StoreURL,
				"cart_id":   params.CartID,
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpUpdateCartTool exposes the MCP update_cart tool to the agent (add items, update quantities, remove items).
type mcpUpdateCartParams struct {
	StoreURL string                  `json:"store_url"`
	CartID   string                  `json:"cart_id,omitempty"`
	AddItems []mcpCartLineItemParams `json:"add_items"`
}

type mcpCartLineItemParams struct {
	ProductVariantID string `json:"product_variant_id"`
	Quantity         int    `json:"quantity"`
}

func mcpUpdateCartTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"update_cart",
		"Add items to a cart, update quantities, or remove items (set quantity to 0). Creates a new cart if no cart_id is provided. Returns the cart ID and checkout URL.",
		func(ctx context.Context, params mcpUpdateCartParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			if len(params.AddItems) == 0 {
				return "", fmt.Errorf("add_items is required")
			}

			// Convert items to the format expected by MCP
			items := make([]map[string]interface{}, len(params.AddItems))
			for i, item := range params.AddItems {
				items[i] = map[string]interface{}{
					"item": map[string]string{
						"id": item.ProductVariantID,
					},
					"quantity": item.Quantity,
				}
			}

			args := map[string]interface{}{
				"store_url": params.StoreURL,
				"add_items": items,
			}
			if params.CartID != "" {
				args["cart_id"] = params.CartID
			}

			out, err := callMCP(ctx, "update_cart", args)
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpCreateCheckoutTool exposes the MCP create_checkout tool to the agent.
type mcpCreateCheckoutParams struct {
	StoreURL  string                `json:"store_url"`
	Currency  string                `json:"currency"`
	LineItems []mcpCheckoutLineItem `json:"line_items"`
	Buyer     *mcpBuyerInfo         `json:"buyer,omitempty"`
}

type mcpCheckoutLineItem struct {
	Item     mcpCheckoutItem `json:"item"`
	Quantity int             `json:"quantity"`
}

type mcpCheckoutItem struct {
	ID string `json:"id"`
}

type mcpBuyerInfo struct {
	Email string `json:"email,omitempty"`
}

func mcpCreateCheckoutTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"create_checkout",
		"Create a new checkout session for completing a purchase. Use this when the customer is ready to buy. Returns a checkout URL to complete payment.",
		func(ctx context.Context, params mcpCreateCheckoutParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			if params.Currency == "" {
				return "", fmt.Errorf("currency is required")
			}
			if len(params.LineItems) == 0 {
				return "", fmt.Errorf("line_items is required")
			}

			// Convert line items to MCP format
			items := make([]map[string]interface{}, len(params.LineItems))
			for i, item := range params.LineItems {
				items[i] = map[string]interface{}{
					"item": map[string]string{
						"id": item.Item.ID,
					},
					"quantity": item.Quantity,
				}
			}

			checkoutData := map[string]interface{}{
				"currency":   params.Currency,
				"line_items": items,
			}

			if params.Buyer != nil && params.Buyer.Email != "" {
				checkoutData["buyer"] = map[string]string{
					"email": params.Buyer.Email,
				}
			}

			out, err := callMCP(ctx, "create_checkout", map[string]interface{}{
				"store_url": params.StoreURL,
				"checkout":  checkoutData,
			})
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}

// mcpSearchShopPoliciesTool exposes the MCP search_shop_policies_and_faqs tool to the agent.
type mcpSearchShopPoliciesParams struct {
	StoreURL string `json:"store_url"`
	Query    string `json:"query"`
	Context  string `json:"context,omitempty"`
}

func mcpSearchShopPoliciesTool() agents.FunctionTool {
	return agents.NewFunctionTool(
		"search_shop_policies",
		"Search a store's policies and FAQs (return policy, shipping info, etc.). Use this when the customer asks about store policies.",
		func(ctx context.Context, params mcpSearchShopPoliciesParams) (string, error) {
			params.StoreURL = strings.TrimSpace(params.StoreURL)
			params.Query = strings.TrimSpace(params.Query)
			if params.StoreURL == "" {
				return "", fmt.Errorf("store_url is required")
			}
			if params.Query == "" {
				return "", fmt.Errorf("query is required")
			}

			args := map[string]interface{}{
				"store_url": params.StoreURL,
				"query":     params.Query,
			}
			if params.Context != "" {
				args["context"] = params.Context
			}

			out, err := callMCP(ctx, "search_shop_policies_and_faqs", args)
			if err != nil {
				return "", err
			}
			return out, nil
		},
	)
}
