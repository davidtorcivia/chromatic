package models

import (
	"time"
)

// Room represents a streaming session room
type Room struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`

	// Scheduling
	ScheduledAt     *time.Time `json:"scheduledAt,omitempty"`
	DurationMinutes *int       `json:"durationMinutes,omitempty"`

	// Access Control
	PasswordHash       *string `json:"-"`
	HasPassword        bool    `json:"hasPassword"`
	WaitingRoomEnabled bool    `json:"waitingRoomEnabled"`

	// Stream Key Binding
	StreamKeyID *string `json:"streamKeyId,omitempty"`

	// Watermark Config
	WatermarkMode         string  `json:"watermarkMode"`
	WatermarkText         *string `json:"watermarkText,omitempty"`
	WatermarkLogoPath     *string `json:"watermarkLogoPath,omitempty"`
	WatermarkLogoPosition string  `json:"watermarkLogoPosition"`
	WatermarkOpacity      float64 `json:"watermarkOpacity"`

	// State
	Status    RoomStatus `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// RoomStatus represents the current state of a room
type RoomStatus string

const (
	RoomStatusPending RoomStatus = "pending"
	RoomStatusLive    RoomStatus = "live"
	RoomStatusEnded   RoomStatus = "ended"
)

// StreamKey represents an OBS stream key for WHIP authentication
type StreamKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyToken  string    `json:"keyToken"`
	CreatedAt time.Time `json:"createdAt"`
}

// Participant represents a user in a room session
type Participant struct {
	ID     string          `json:"id"`
	RoomID string          `json:"roomId"`
	Name   string          `json:"name"`
	Role   ParticipantRole `json:"role"`
	Color  string          `json:"color"`

	IsAdmitted   bool      `json:"isAdmitted"`
	JoinedAt     time.Time `json:"joinedAt"`
	AudioEnabled bool      `json:"audioEnabled"`
	VideoEnabled bool      `json:"videoEnabled"`
	// CanScreenshare records a persistent admin approval for screen sharing.
	// Admins are implicitly allowed regardless of this flag.
	CanScreenshare bool `json:"canScreenshare"`
}

// ParticipantRole defines the role of a participant
type ParticipantRole string

const (
	RoleAdmin  ParticipantRole = "admin"
	RoleViewer ParticipantRole = "viewer"
)

// Message represents a chat message
type Message struct {
	ID            string      `json:"id"`
	RoomID        string      `json:"roomId"`
	ParticipantID string      `json:"participantId"`
	Type          MessageType `json:"type"`
	Content       string      `json:"content"`
	CreatedAt     time.Time   `json:"createdAt"`

	// Joined fields (not stored directly)
	ParticipantName string `json:"participantName,omitempty"`
}

// MessageType defines the type of message
type MessageType string

const (
	MessageTypeText MessageType = "text"
	MessageTypeFile MessageType = "file"
)

// File represents an uploaded file
type File struct {
	ID           string    `json:"id"`
	RoomID       string    `json:"roomId"`
	UploaderID   string    `json:"uploaderId"`
	OriginalName string    `json:"originalName"`
	StoredPath   string    `json:"-"`
	MimeType     string    `json:"mimeType"`
	SizeBytes    int64     `json:"sizeBytes"`
	CreatedAt    time.Time `json:"createdAt"`

	// Computed URLs
	URL          string `json:"url,omitempty"`
	ThumbnailURL string `json:"thumbnailUrl,omitempty"`
}

// Config represents global application configuration stored in database
type Config struct {
	DefaultWatermarkText     *string `json:"defaultWatermarkText,omitempty"`
	DefaultWatermarkLogoPath *string `json:"defaultWatermarkLogoPath,omitempty"`
	TurnExternalURL          *string `json:"turnExternalUrl,omitempty"`
	TurnExternalUsername     *string `json:"turnExternalUsername,omitempty"`
	TurnExternalCredential   *string `json:"-"`
}
