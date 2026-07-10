// Package models defines the database-backed domain types shared by the
// HTTP handlers.
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
	// EarlyOpenMinutes is how many minutes before ScheduledAt guests may
	// enter the countdown lobby (0-120, default 10).
	EarlyOpenMinutes int `json:"earlyOpenMinutes"`
	// OpenedAt is set when an admin opens the room ahead of schedule or when
	// the first stream arrives. Nil = the room opens at ScheduledAt.
	OpenedAt *time.Time `json:"openedAt,omitempty"`

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
	// WatermarkPosX/Y are the center of the watermark as a fraction (0-1) of
	// the video width/height. Nil = legacy built-in placement.
	WatermarkPosX *float64 `json:"watermarkPosX,omitempty"`
	WatermarkPosY *float64 `json:"watermarkPosY,omitempty"`
	// WatermarkScale multiplies the base text/logo size (0.25-3.0, 1.0 = default).
	WatermarkScale float64 `json:"watermarkScale"`

	// MaxParticipants overrides the global participant cap when set (1-100).
	// Nil = use the global MaxParticipantsPerRoom default.
	MaxParticipants *int `json:"maxParticipants,omitempty"`

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
