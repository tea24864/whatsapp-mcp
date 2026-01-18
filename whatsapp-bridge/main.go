package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"

	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	dsn := "file:store/messages.db?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=10000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to ping message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat_timestamp ON messages(chat_jid, timestamp);
		CREATE INDEX IF NOT EXISTS idx_messages_sender_timestamp ON messages(sender, timestamp);
		CREATE INDEX IF NOT EXISTS idx_chats_last_message_time ON chats(last_message_time);
	`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

const apiTimeLayout = "2006-01-02T15:04:05.999999999-07:00"

func formatAPITime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(apiTimeLayout)
}

func parseAPITime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}

	for _, layout := range layouts {
		if strings.Contains(layout, "Z07:00") {
			parsed, err := time.Parse(layout, s)
			if err == nil {
				return parsed, nil
			}
			continue
		}

		parsed, err := time.ParseInLocation(layout, s, time.UTC)
		if err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, fmt.Errorf("invalid time format: %s", s)
}

func normalizeLimit(limit int, defaultLimit int, maxLimit int) int {
	if limit <= 0 {
		return defaultLimit
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}

func normalizePage(page int) int {
	if page < 0 {
		return 0
	}
	return page
}

func (store *MessageStore) GetSenderName(senderJID string) (string, error) {
	var name string

	err := store.db.QueryRow("SELECT name FROM chats WHERE jid = ? LIMIT 1", senderJID).Scan(&name)
	if err == nil && name != "" {
		return name, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	phonePart := senderJID
	if strings.Contains(senderJID, "@") {
		phonePart = strings.SplitN(senderJID, "@", 2)[0]
	}

	pattern := "%" + phonePart + "%"
	err = store.db.QueryRow("SELECT name FROM chats WHERE jid LIKE ? LIMIT 1", pattern).Scan(&name)
	if err == nil && name != "" {
		return name, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}

	return "", nil
}

func (store *MessageStore) SearchContacts(query string) ([]ContactDTO, error) {
	pattern := "%" + query + "%"
	rows, err := store.db.Query(
		`SELECT DISTINCT jid, name
		 FROM chats
		 WHERE (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
		   AND jid NOT LIKE '%@g.us'
		 ORDER BY name, jid
		 LIMIT 50`,
		pattern, pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contacts []ContactDTO
	for rows.Next() {
		var jid string
		var name sql.NullString
		if err := rows.Scan(&jid, &name); err != nil {
			return nil, err
		}

		phone := jid
		if strings.Contains(jid, "@") {
			phone = strings.SplitN(jid, "@", 2)[0]
		}

		c := ContactDTO{PhoneNumber: phone, JID: jid}
		if name.Valid {
			c.Name = name.String
		}
		contacts = append(contacts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return contacts, nil
}

func (store *MessageStore) ListChats(query string, limit int, page int, includeLastMessage bool, sortBy string) ([]ChatDTO, error) {
	limit = normalizeLimit(limit, 20, 100)
	page = normalizePage(page)
	offset := page * limit

	var qb strings.Builder
	qb.WriteString("SELECT chats.jid, chats.name, chats.last_message_time")
	if includeLastMessage {
		qb.WriteString(", messages.content as last_message, messages.sender as last_sender, messages.is_from_me as last_is_from_me")
	} else {
		qb.WriteString(", NULL as last_message, NULL as last_sender, NULL as last_is_from_me")
	}
	qb.WriteString(" FROM chats")
	if includeLastMessage {
		qb.WriteString(" LEFT JOIN messages ON chats.jid = messages.chat_jid AND chats.last_message_time = messages.timestamp")
	}

	params := make([]any, 0, 4)
	if query != "" {
		qb.WriteString(" WHERE (LOWER(chats.name) LIKE LOWER(?) OR chats.jid LIKE ?)")
		pattern := "%" + query + "%"
		params = append(params, pattern, pattern)
	}

	orderBy := "chats.last_message_time DESC"
	if sortBy == "name" {
		orderBy = "chats.name"
	}
	qb.WriteString(" ORDER BY " + orderBy)
	qb.WriteString(" LIMIT ? OFFSET ?")
	params = append(params, limit, offset)

	rows, err := store.db.Query(qb.String(), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChatDTO
	for rows.Next() {
		var jid string
		var name sql.NullString
		var lastMessageTime sql.NullTime
		var lastMessage sql.NullString
		var lastSender sql.NullString
		var lastIsFromMe sql.NullBool
		if err := rows.Scan(&jid, &name, &lastMessageTime, &lastMessage, &lastSender, &lastIsFromMe); err != nil {
			return nil, err
		}

		c := ChatDTO{JID: jid}
		if name.Valid {
			c.Name = name.String
		}
		if lastMessageTime.Valid {
			c.LastMessageTime = formatAPITime(lastMessageTime.Time)
		}
		if lastMessage.Valid {
			c.LastMessage = lastMessage.String
		}
		if lastSender.Valid {
			c.LastSender = lastSender.String
		}
		if lastIsFromMe.Valid {
			v := lastIsFromMe.Bool
			c.LastIsFromMe = &v
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (store *MessageStore) GetChat(chatJID string, includeLastMessage bool) (*ChatDTO, error) {
	req := ListChatsRequest{IncludeLastMessage: includeLastMessage}
	_ = req

	query := "SELECT c.jid, c.name, c.last_message_time"
	if includeLastMessage {
		query += ", m.content as last_message, m.sender as last_sender, m.is_from_me as last_is_from_me"
	} else {
		query += ", NULL as last_message, NULL as last_sender, NULL as last_is_from_me"
	}
	query += " FROM chats c"
	if includeLastMessage {
		query += " LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp"
	}
	query += " WHERE c.jid = ?"

	var jid string
	var name sql.NullString
	var lastMessageTime sql.NullTime
	var lastMessage sql.NullString
	var lastSender sql.NullString
	var lastIsFromMe sql.NullBool
	err := store.db.QueryRow(query, chatJID).Scan(&jid, &name, &lastMessageTime, &lastMessage, &lastSender, &lastIsFromMe)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c := &ChatDTO{JID: jid}
	if name.Valid {
		c.Name = name.String
	}
	if lastMessageTime.Valid {
		c.LastMessageTime = formatAPITime(lastMessageTime.Time)
	}
	if lastMessage.Valid {
		c.LastMessage = lastMessage.String
	}
	if lastSender.Valid {
		c.LastSender = lastSender.String
	}
	if lastIsFromMe.Valid {
		v := lastIsFromMe.Bool
		c.LastIsFromMe = &v
	}
	return c, nil
}

func (store *MessageStore) GetDirectChatByContact(senderPhoneNumber string) (*ChatDTO, error) {
	pattern := "%" + senderPhoneNumber + "%"
	query := `SELECT c.jid, c.name, c.last_message_time,
		       m.content as last_message, m.sender as last_sender, m.is_from_me as last_is_from_me
		  FROM chats c
		  LEFT JOIN messages m ON c.jid = m.chat_jid AND c.last_message_time = m.timestamp
		 WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
		 LIMIT 1`

	var jid string
	var name sql.NullString
	var lastMessageTime sql.NullTime
	var lastMessage sql.NullString
	var lastSender sql.NullString
	var lastIsFromMe sql.NullBool
	err := store.db.QueryRow(query, pattern).Scan(&jid, &name, &lastMessageTime, &lastMessage, &lastSender, &lastIsFromMe)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c := &ChatDTO{JID: jid}
	if name.Valid {
		c.Name = name.String
	}
	if lastMessageTime.Valid {
		c.LastMessageTime = formatAPITime(lastMessageTime.Time)
	}
	if lastMessage.Valid {
		c.LastMessage = lastMessage.String
	}
	if lastSender.Valid {
		c.LastSender = lastSender.String
	}
	if lastIsFromMe.Valid {
		v := lastIsFromMe.Bool
		c.LastIsFromMe = &v
	}
	return c, nil
}

func (store *MessageStore) GetContactChats(jid string, limit int, page int) ([]ChatDTO, error) {
	limit = normalizeLimit(limit, 20, 100)
	page = normalizePage(page)
	offset := page * limit

	query := `SELECT DISTINCT
		       c.jid,
		       c.name,
		       c.last_message_time,
		       m.content as last_message,
		       m.sender as last_sender,
		       m.is_from_me as last_is_from_me
		  FROM chats c
		  JOIN messages m ON c.jid = m.chat_jid
		 WHERE m.sender = ? OR c.jid = ?
		 ORDER BY c.last_message_time DESC
		 LIMIT ? OFFSET ?`

	rows, err := store.db.Query(query, jid, jid, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChatDTO
	for rows.Next() {
		var chatJID string
		var name sql.NullString
		var lastMessageTime sql.NullTime
		var lastMessage sql.NullString
		var lastSender sql.NullString
		var lastIsFromMe sql.NullBool
		if err := rows.Scan(&chatJID, &name, &lastMessageTime, &lastMessage, &lastSender, &lastIsFromMe); err != nil {
			return nil, err
		}

		c := ChatDTO{JID: chatJID}
		if name.Valid {
			c.Name = name.String
		}
		if lastMessageTime.Valid {
			c.LastMessageTime = formatAPITime(lastMessageTime.Time)
		}
		if lastMessage.Valid {
			c.LastMessage = lastMessage.String
		}
		if lastSender.Valid {
			c.LastSender = lastSender.String
		}
		if lastIsFromMe.Valid {
			v := lastIsFromMe.Bool
			c.LastIsFromMe = &v
		}

		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (store *MessageStore) GetLastInteraction(jid string) (*MessageDTO, error) {
	query := `SELECT 
		       m.timestamp,
		       m.sender,
		       c.name,
		       m.content,
		       m.is_from_me,
		       c.jid,
		       m.id,
		       m.media_type,
		       m.filename
		  FROM messages m
		  JOIN chats c ON m.chat_jid = c.jid
		 WHERE m.sender = ? OR c.jid = ?
		 ORDER BY m.timestamp DESC
		 LIMIT 1`

	var ts time.Time
	var sender string
	var chatName sql.NullString
	var content sql.NullString
	var isFromMe bool
	var chatJID string
	var id string
	var mediaType sql.NullString
	var filename sql.NullString
	err := store.db.QueryRow(query, jid, jid).Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJID, &id, &mediaType, &filename)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	m := &MessageDTO{
		ID:        id,
		ChatJID:   chatJID,
		Timestamp: formatAPITime(ts),
		Sender:    sender,
		IsFromMe:  isFromMe,
	}
	if chatName.Valid {
		m.ChatName = chatName.String
	}
	if content.Valid {
		m.Content = content.String
	}
	if mediaType.Valid {
		m.MediaType = mediaType.String
	}
	if filename.Valid {
		m.Filename = filename.String
	}
	return m, nil
}

func (store *MessageStore) GetMessageContext(messageID string, chatJID string, before int, after int) (*MessageContextDTO, error) {
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}
	if before > 50 {
		before = 50
	}
	if after > 50 {
		after = 50
	}

	baseQuery := `SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.chat_jid, m.media_type, m.filename
		         FROM messages m
		         JOIN chats c ON m.chat_jid = c.jid`

	var row *sql.Row
	if chatJID != "" {
		row = store.db.QueryRow(baseQuery+" WHERE m.id = ? AND m.chat_jid = ? LIMIT 1", messageID, chatJID)
	} else {
		row = store.db.QueryRow(baseQuery+" WHERE m.id = ? ORDER BY m.timestamp DESC LIMIT 1", messageID)
	}

	var ts time.Time
	var sender string
	var chatName sql.NullString
	var content sql.NullString
	var isFromMe bool
	var chatJIDResolved string
	var id string
	var chatJIDFromMsg string
	var mediaType sql.NullString
	var filename sql.NullString
	err := row.Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJIDResolved, &id, &chatJIDFromMsg, &mediaType, &filename)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	msg := MessageDTO{
		ID:        id,
		ChatJID:   chatJIDFromMsg,
		Timestamp: formatAPITime(ts),
		Sender:    sender,
		IsFromMe:  isFromMe,
	}
	if chatName.Valid {
		msg.ChatName = chatName.String
	}
	if content.Valid {
		msg.Content = content.String
	}
	if mediaType.Valid {
		msg.MediaType = mediaType.String
	}
	if filename.Valid {
		msg.Filename = filename.String
	}

	name, err := store.GetSenderName(sender)
	if err == nil && name != "" {
		msg.SenderName = name
	}

	beforeRows, err := store.db.Query(
		`SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type, m.filename
		   FROM messages m
		   JOIN chats c ON m.chat_jid = c.jid
		  WHERE m.chat_jid = ? AND m.timestamp < ?
		  ORDER BY m.timestamp DESC
		  LIMIT ?`,
		chatJIDFromMsg, ts, before,
	)
	if err != nil {
		return nil, err
	}
	defer beforeRows.Close()

	beforeMsgs := make([]MessageDTO, 0, before)
	for beforeRows.Next() {
		var bts time.Time
		var bsender string
		var bchatName sql.NullString
		var bcontent sql.NullString
		var bisFromMe bool
		var bchatJID string
		var bid string
		var bmediaType sql.NullString
		var bfilename sql.NullString
		if err := beforeRows.Scan(&bts, &bsender, &bchatName, &bcontent, &bisFromMe, &bchatJID, &bid, &bmediaType, &bfilename); err != nil {
			return nil, err
		}
		m := MessageDTO{ID: bid, ChatJID: bchatJID, Timestamp: formatAPITime(bts), Sender: bsender, IsFromMe: bisFromMe}
		if bchatName.Valid {
			m.ChatName = bchatName.String
		}
		if bcontent.Valid {
			m.Content = bcontent.String
		}
		if bmediaType.Valid {
			m.MediaType = bmediaType.String
		}
		if bfilename.Valid {
			m.Filename = bfilename.String
		}
		if name, err := store.GetSenderName(bsender); err == nil && name != "" {
			m.SenderName = name
		}
		beforeMsgs = append(beforeMsgs, m)
	}
	if err := beforeRows.Err(); err != nil {
		return nil, err
	}

	afterRows, err := store.db.Query(
		`SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type, m.filename
		   FROM messages m
		   JOIN chats c ON m.chat_jid = c.jid
		  WHERE m.chat_jid = ? AND m.timestamp > ?
		  ORDER BY m.timestamp ASC
		  LIMIT ?`,
		chatJIDFromMsg, ts, after,
	)
	if err != nil {
		return nil, err
	}
	defer afterRows.Close()

	afterMsgs := make([]MessageDTO, 0, after)
	for afterRows.Next() {
		var ats time.Time
		var asender string
		var achatName sql.NullString
		var acontent sql.NullString
		var aisFromMe bool
		var achatJID string
		var aid string
		var amediaType sql.NullString
		var afilename sql.NullString
		if err := afterRows.Scan(&ats, &asender, &achatName, &acontent, &aisFromMe, &achatJID, &aid, &amediaType, &afilename); err != nil {
			return nil, err
		}
		m := MessageDTO{ID: aid, ChatJID: achatJID, Timestamp: formatAPITime(ats), Sender: asender, IsFromMe: aisFromMe}
		if achatName.Valid {
			m.ChatName = achatName.String
		}
		if acontent.Valid {
			m.Content = acontent.String
		}
		if amediaType.Valid {
			m.MediaType = amediaType.String
		}
		if afilename.Valid {
			m.Filename = afilename.String
		}
		if name, err := store.GetSenderName(asender); err == nil && name != "" {
			m.SenderName = name
		}
		afterMsgs = append(afterMsgs, m)
	}
	if err := afterRows.Err(); err != nil {
		return nil, err
	}

	ctx := &MessageContextDTO{Message: msg, Before: beforeMsgs, After: afterMsgs}
	return ctx, nil
}

func (store *MessageStore) ListMessages(req ListMessagesRequest) ([]MessageDTO, error) {
	limit := normalizeLimit(req.Limit, 20, 100)
	page := normalizePage(req.Page)
	offset := page * limit
	includeContext := req.IncludeContext
	contextBefore := req.ContextBefore
	contextAfter := req.ContextAfter
	if contextBefore <= 0 {
		contextBefore = 1
	}
	if contextAfter <= 0 {
		contextAfter = 1
	}

	var qb strings.Builder
	qb.WriteString("SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type, m.filename FROM messages m")
	qb.WriteString(" JOIN chats c ON m.chat_jid = c.jid")

	where := make([]string, 0, 5)
	params := make([]any, 0, 7)

	if req.After != "" {
		t, err := parseAPITime(req.After)
		if err != nil {
			return nil, err
		}
		where = append(where, "m.timestamp > ?")
		params = append(params, t)
	}
	if req.Before != "" {
		t, err := parseAPITime(req.Before)
		if err != nil {
			return nil, err
		}
		where = append(where, "m.timestamp < ?")
		params = append(params, t)
	}
	if req.SenderPhoneNumber != "" {
		where = append(where, "m.sender = ?")
		params = append(params, req.SenderPhoneNumber)
	}
	if req.ChatJID != "" {
		where = append(where, "m.chat_jid = ?")
		params = append(params, req.ChatJID)
	}
	if req.Query != "" {
		where = append(where, "LOWER(m.content) LIKE LOWER(?)")
		params = append(params, "%"+req.Query+"%")
	}
	if len(where) > 0 {
		qb.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	qb.WriteString(" ORDER BY m.timestamp DESC, m.id DESC, m.chat_jid DESC")
	qb.WriteString(" LIMIT ? OFFSET ?")
	params = append(params, limit, offset)

	rows, err := store.db.Query(qb.String(), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	base := make([]MessageDTO, 0, limit)
	for rows.Next() {
		var ts time.Time
		var sender string
		var chatName sql.NullString
		var content sql.NullString
		var isFromMe bool
		var chatJID string
		var id string
		var mediaType sql.NullString
		var filename sql.NullString
		if err := rows.Scan(&ts, &sender, &chatName, &content, &isFromMe, &chatJID, &id, &mediaType, &filename); err != nil {
			return nil, err
		}

		m := MessageDTO{ID: id, ChatJID: chatJID, Timestamp: formatAPITime(ts), Sender: sender, IsFromMe: isFromMe}
		if chatName.Valid {
			m.ChatName = chatName.String
		}
		if content.Valid {
			m.Content = content.String
		}
		if mediaType.Valid {
			m.MediaType = mediaType.String
		}
		if filename.Valid {
			m.Filename = filename.String
		}
		if name, err := store.GetSenderName(sender); err == nil && name != "" {
			m.SenderName = name
		}

		base = append(base, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if !includeContext {
		return base, nil
	}

	out := make([]MessageDTO, 0, len(base)*(1+contextBefore+contextAfter))
	for _, m := range base {
		ctx, err := store.GetMessageContext(m.ID, m.ChatJID, contextBefore, contextAfter)
		if err != nil {
			return nil, err
		}
		if ctx == nil {
			continue
		}
		out = append(out, ctx.Before...)
		out = append(out, ctx.Message)
		out = append(out, ctx.After...)
	}

	return out, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

const apiKeyHeader = "X-API-Key"

type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		fmt.Println("Media uploaded", resp)

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	_, err = client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

type MessageDTO struct {
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	ChatName   string `json:"chat_name,omitempty"`
	Timestamp  string `json:"timestamp"`
	Sender     string `json:"sender"`
	SenderName string `json:"sender_name,omitempty"`
	Content    string `json:"content"`
	IsFromMe   bool   `json:"is_from_me"`
	MediaType  string `json:"media_type,omitempty"`
	Filename   string `json:"filename,omitempty"`
}

type ChatDTO struct {
	JID             string `json:"jid"`
	Name            string `json:"name,omitempty"`
	LastMessageTime string `json:"last_message_time,omitempty"`
	LastMessage     string `json:"last_message,omitempty"`
	LastSender      string `json:"last_sender,omitempty"`
	LastIsFromMe    *bool  `json:"last_is_from_me,omitempty"`
}

type ContactDTO struct {
	PhoneNumber string `json:"phone_number"`
	Name        string `json:"name,omitempty"`
	JID         string `json:"jid"`
}

type MessageContextDTO struct {
	Message MessageDTO   `json:"message"`
	Before  []MessageDTO `json:"before"`
	After   []MessageDTO `json:"after"`
}

type SearchContactsRequest struct {
	Query string `json:"query"`
}

type SearchContactsResponse struct {
	Success  bool         `json:"success"`
	Message  string       `json:"message,omitempty"`
	Contacts []ContactDTO `json:"contacts"`
}

type ListChatsRequest struct {
	Query              string `json:"query,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	Page               int    `json:"page,omitempty"`
	IncludeLastMessage bool   `json:"include_last_message,omitempty"`
	SortBy             string `json:"sort_by,omitempty"`
}

type ListChatsResponse struct {
	Success bool      `json:"success"`
	Message string    `json:"message,omitempty"`
	Chats   []ChatDTO `json:"chats"`
}

type GetChatRequest struct {
	ChatJID            string `json:"chat_jid"`
	IncludeLastMessage bool   `json:"include_last_message,omitempty"`
}

type GetChatResponse struct {
	Success bool     `json:"success"`
	Message string   `json:"message,omitempty"`
	Chat    *ChatDTO `json:"chat"`
}

type GetDirectChatByContactRequest struct {
	SenderPhoneNumber string `json:"sender_phone_number"`
}

type GetDirectChatByContactResponse struct {
	Success bool     `json:"success"`
	Message string   `json:"message,omitempty"`
	Chat    *ChatDTO `json:"chat"`
}

type GetContactChatsRequest struct {
	JID   string `json:"jid"`
	Limit int    `json:"limit,omitempty"`
	Page  int    `json:"page,omitempty"`
}

type GetContactChatsResponse struct {
	Success bool      `json:"success"`
	Message string    `json:"message,omitempty"`
	Chats   []ChatDTO `json:"chats"`
}

type GetLastInteractionRequest struct {
	JID string `json:"jid"`
}

type GetLastInteractionResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Last    *MessageDTO `json:"last"`
}

type ListMessagesRequest struct {
	After             string `json:"after,omitempty"`
	Before            string `json:"before,omitempty"`
	SenderPhoneNumber string `json:"sender_phone_number,omitempty"`
	ChatJID           string `json:"chat_jid,omitempty"`
	Query             string `json:"query,omitempty"`
	Limit             int    `json:"limit,omitempty"`
	Page              int    `json:"page,omitempty"`
	IncludeContext    bool   `json:"include_context,omitempty"`
	ContextBefore     int    `json:"context_before,omitempty"`
	ContextAfter      int    `json:"context_after,omitempty"`
}

type ListMessagesResponse struct {
	Success  bool         `json:"success"`
	Message  string       `json:"message,omitempty"`
	Messages []MessageDTO `json:"messages"`
}

type GetMessageContextRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid,omitempty"`
	Before    int    `json:"before,omitempty"`
	After     int    `json:"after,omitempty"`
}

type GetMessageContextResponse struct {
	Success bool               `json:"success"`
	Message string             `json:"message,omitempty"`
	Context *MessageContextDTO `json:"context"`
}

func requireAPIKey(apiKey string, w http.ResponseWriter, r *http.Request, logger waLog.Logger) bool {
	if apiKey == "" {
		http.Error(w, "API key is not configured", http.StatusInternalServerError)
		return false
	}

	provided := r.Header.Get(apiKeyHeader)
	if provided == "" {
		logger.Warnf("AUTHZ_DENIED")
		http.Error(w, "Missing API key", http.StatusUnauthorized)
		return false
	}

	if subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
		logger.Warnf("AUTHZ_DENIED")
		http.Error(w, "Invalid API key", http.StatusUnauthorized)
		return false
	}

	return true
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	localPath = fmt.Sprintf("%s/%s", chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	// Remove query parameters
	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	// Create proper direct path format
	return "/" + pathPart
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int, apiKey string, tlsCertPath string, tlsKeyPath string, logger waLog.Logger) {
	if tlsCertPath == "" || tlsKeyPath == "" {
		fmt.Println("TLS certificate or key path is missing; HTTPS server not started")
		return
	}

	if _, err := os.Stat(tlsCertPath); err != nil {
		fmt.Printf("TLS certificate not found at %s: %v\n", tlsCertPath, err)
		return
	}

	if _, err := os.Stat(tlsKeyPath); err != nil {
		fmt.Printf("TLS key not found at %s: %v\n", tlsKeyPath, err)
		return
	}
	// Handler for sending messages
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		success, message := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	http.HandleFunc("/api/search-contacts", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req SearchContactsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		contacts, err := messageStore.SearchContacts(req.Query)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(SearchContactsResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err)})
			return
		}

		json.NewEncoder(w).Encode(SearchContactsResponse{Success: true, Contacts: contacts})
	})

	http.HandleFunc("/api/list-chats", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ListChatsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		chats, err := messageStore.ListChats(req.Query, req.Limit, req.Page, req.IncludeLastMessage, req.SortBy)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(ListChatsResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err)})
			return
		}

		json.NewEncoder(w).Encode(ListChatsResponse{Success: true, Chats: chats})
	})

	http.HandleFunc("/api/get-chat", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req GetChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.ChatJID == "" {
			http.Error(w, "chat_jid is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		chat, err := messageStore.GetChat(req.ChatJID, req.IncludeLastMessage)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GetChatResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Chat: nil})
			return
		}

		json.NewEncoder(w).Encode(GetChatResponse{Success: true, Chat: chat})
	})

	http.HandleFunc("/api/get-direct-chat-by-contact", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req GetDirectChatByContactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.SenderPhoneNumber == "" {
			http.Error(w, "sender_phone_number is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		chat, err := messageStore.GetDirectChatByContact(req.SenderPhoneNumber)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GetDirectChatByContactResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Chat: nil})
			return
		}

		json.NewEncoder(w).Encode(GetDirectChatByContactResponse{Success: true, Chat: chat})
	})

	http.HandleFunc("/api/get-contact-chats", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req GetContactChatsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.JID == "" {
			http.Error(w, "jid is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		chats, err := messageStore.GetContactChats(req.JID, req.Limit, req.Page)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GetContactChatsResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Chats: nil})
			return
		}

		json.NewEncoder(w).Encode(GetContactChatsResponse{Success: true, Chats: chats})
	})

	http.HandleFunc("/api/get-last-interaction", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req GetLastInteractionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.JID == "" {
			http.Error(w, "jid is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		last, err := messageStore.GetLastInteraction(req.JID)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GetLastInteractionResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Last: nil})
			return
		}

		json.NewEncoder(w).Encode(GetLastInteractionResponse{Success: true, Last: last})
	})

	http.HandleFunc("/api/list-messages", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ListMessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		messages, err := messageStore.ListMessages(req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(ListMessagesResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Messages: nil})
			return
		}

		json.NewEncoder(w).Encode(ListMessagesResponse{Success: true, Messages: messages})
	})

	http.HandleFunc("/api/get-message-context", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req GetMessageContextRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.MessageID == "" {
			http.Error(w, "message_id is required", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		context, err := messageStore.GetMessageContext(req.MessageID, req.ChatJID, req.Before, req.After)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GetMessageContextResponse{Success: false, Message: fmt.Sprintf("Database error: %v", err), Context: nil})
			return
		}

		json.NewEncoder(w).Encode(GetMessageContextResponse{Success: true, Context: context})
	})

	// Handler for downloading media
	http.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		if !requireAPIKey(apiKey, w, r, logger) {
			return
		}
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Start the server
	serverAddr := fmt.Sprintf(":%d", port)
	fmt.Printf("Starting REST API server on https://localhost%s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServeTLS(serverAddr, tlsCertPath, tlsKeyPath, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	if err := godotenv.Load(); err != nil {
		logger.Infof("No .env file loaded: %v", err)
	}

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	apiKey := os.Getenv("WHATSAPP_BRIDGE_API_KEY")
	if apiKey == "" {
		logger.Errorf("WHATSAPP_BRIDGE_API_KEY is not set; refusing to start REST API server")
		return
	}

	tlsCertPath := os.Getenv("WHATSAPP_BRIDGE_TLS_CERT")
	tlsKeyPath := os.Getenv("WHATSAPP_BRIDGE_TLS_KEY")
	if tlsCertPath == "" {
		tlsCertPath = "store/server.crt"
	}
	if tlsKeyPath == "" {
		tlsKeyPath = "store/server.key"
	}

	// Start REST API server
	startRESTServer(client, messageStore, 8080, apiKey, tlsCertPath, tlsKeyPath, logger)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
