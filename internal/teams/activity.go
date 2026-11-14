// Package teams implements the Bot Framework / Teams side of the gateway: inbound
// token validation, the outbound Connector client and the Teams rendering sink.
package teams

import (
	"encoding/json"
	"strings"
)

// Activity is the subset of the Bot Framework Activity schema the gateway uses.
type Activity struct {
	Type         string            `json:"type"`
	ID           string            `json:"id,omitempty"`
	Timestamp    string            `json:"timestamp,omitempty"`
	ServiceURL   string            `json:"serviceUrl,omitempty"`
	ChannelID    string            `json:"channelId,omitempty"`
	From         *ChannelAccount   `json:"from,omitempty"`
	Conversation *ConversationRef  `json:"conversation,omitempty"`
	Recipient    *ChannelAccount   `json:"recipient,omitempty"`
	TextFormat   string            `json:"textFormat,omitempty"`
	Text         string            `json:"text,omitempty"`
	Locale       string            `json:"locale,omitempty"`
	ReplyToID    string            `json:"replyToId,omitempty"`
	Attachments  []Attachment      `json:"attachments,omitempty"`
	Entities     []json.RawMessage `json:"entities,omitempty"`
	ChannelData  json.RawMessage   `json:"channelData,omitempty"`
	Value        json.RawMessage   `json:"value,omitempty"`
	Name         string            `json:"name,omitempty"`
	MembersAdded []ChannelAccount  `json:"membersAdded,omitempty"`
}

type ChannelAccount struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
	Role        string `json:"role,omitempty"`
}

type ConversationRef struct {
	ID               string `json:"id"`
	ConversationType string `json:"conversationType,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
	IsGroup          bool   `json:"isGroup,omitempty"`
	Name             string `json:"name,omitempty"`
}

type Attachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl,omitempty"`
	Content     any    `json:"content,omitempty"`
	Name        string `json:"name,omitempty"`
}

const (
	ActivityMessage            = "message"
	ActivityTyping             = "typing"
	ActivityConversationUpdate = "conversationUpdate"
	ActivityInvoke             = "invoke"

	ConversationPersonal = "personal"

	ContentTypeAdaptiveCard = "application/vnd.microsoft.card.adaptive"
)

type teamsChannelData struct {
	Tenant *struct {
		ID string `json:"id"`
	} `json:"tenant,omitempty"`
}

// TenantID returns the Entra tenant of the activity, preferring channelData.tenant.id.
func (a *Activity) TenantID() string {
	if len(a.ChannelData) > 0 {
		var cd teamsChannelData
		if json.Unmarshal(a.ChannelData, &cd) == nil && cd.Tenant != nil && cd.Tenant.ID != "" {
			return cd.Tenant.ID
		}
	}
	if a.Conversation != nil {
		return a.Conversation.TenantID
	}
	return ""
}

func (a *Activity) IsPersonal() bool {
	return a.Conversation != nil && (a.Conversation.ConversationType == ConversationPersonal ||
		(a.Conversation.ConversationType == "" && !a.Conversation.IsGroup))
}

type mentionEntity struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Mentioned *ChannelAccount `json:"mentioned"`
}

// TextWithoutBotMention returns the message text with any @mention of the bot removed.
// In channels and group chats Teams only delivers messages that mention the bot, and
// the mention markup ("<at>Bot</at>") is not part of what the user is asking.
func (a *Activity) TextWithoutBotMention() string {
	text := a.Text
	if a.Recipient != nil {
		for _, raw := range a.Entities {
			var m mentionEntity
			if json.Unmarshal(raw, &m) != nil || !strings.EqualFold(m.Type, "mention") || m.Mentioned == nil {
				continue
			}
			if m.Mentioned.ID == a.Recipient.ID && m.Text != "" {
				text = strings.ReplaceAll(text, m.Text, "")
			}
		}
	}
	return strings.TrimSpace(text)
}

// MessageActivity builds an outbound message.
func MessageActivity(text string) *Activity {
	return &Activity{Type: ActivityMessage, Text: text, TextFormat: "markdown"}
}

// AdaptiveCardActivity builds an outbound message carrying one Adaptive Card.
func AdaptiveCardActivity(card any) *Activity {
	return &Activity{
		Type:        ActivityMessage,
		Attachments: []Attachment{{ContentType: ContentTypeAdaptiveCard, Content: card}},
	}
}

// aiGeneratedEntity labels a message as AI generated in the Teams client.
func aiGeneratedEntity() json.RawMessage {
	return json.RawMessage(`{"type":"https://schema.org/Message","@type":"Message","@context":"https://schema.org","@id":"","additionalType":["AIGeneratedContent"]}`)
}

func streamInfoEntity(streamID, streamType string, seq int) json.RawMessage {
	m := map[string]any{"type": "streaminfo", "streamType": streamType}
	if streamID != "" {
		m["streamId"] = streamID
	}
	if seq > 0 {
		m["streamSequence"] = seq
	}
	b, _ := json.Marshal(m)
	return b
}
