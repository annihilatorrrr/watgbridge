package utils

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"

	"watgbridge/database"
	"watgbridge/state"

	"github.com/lithammer/fuzzysearch/fuzzy"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func WaParseJID(s string) (types.JID, bool) {
	if s[0] == '+' {
		s = SubString(s, 1, len(s)-1)
	}

	if !strings.ContainsRune(s, '@') {
		return types.NewJID(s, types.DefaultUserServer).ToNonAD(), true
	}

	recipient, err := types.ParseJID(s)

	recipient = recipient.ToNonAD()
	if err != nil || recipient.User == "" {
		return recipient, false
	}

	return recipient, true
}

func WaFuzzyFindContacts(query string) (map[string]string, int, error) {
	var (
		results      = make(map[string]string)
		resultsCount = 0
	)

	contacts, err := database.ContactGetAll()
	if err != nil {
		return nil, 0, err
	}

	var searchSpace []string
	for _, contact := range contacts {
		jid := contact.ID
		if contact.FirstName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.FirstName))
		}
		if contact.FullName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.FullName))
		}
		if contact.PushName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.PushName))
		}
		if contact.BusinessName != "" {
			searchSpace = append(searchSpace, jid+"||"+strings.ToLower(contact.BusinessName))
		}
	}

	fuzzyResults := fuzzy.Find(strings.ToLower(query), searchSpace)
	for _, res := range fuzzyResults {
		info := strings.SplitN(res, "||", 2)

		contact := contacts[info[0]]
		if _, exists := results[info[0]]; exists {
			continue
		}

		resultsCount += 1
		name := ""
		if contact.FullName != "" {
			name += (contact.FullName + " (s)")
		}
		if contact.BusinessName != "" {
			if name != "" {
				name += ", "
			}
			name += (contact.BusinessName + " (b)")
		}
		if contact.PushName != "" {
			if name != "" {
				name += ", "
			}
			name += (contact.PushName + " (p)")
		}
		results[contact.ID] = name
	}

	return results, resultsCount, nil
}

func WaGetGroupName(jid types.JID) string {
	waClient := state.State.WhatsAppClient

	groupInfo, err := waClient.GetGroupInfo(jid)
	if err != nil {
		return jid.User
	}
	return groupInfo.Name
}

func WaGetContactName(jid types.JID) string {
	var name string

	firstName, fullName, pushName, businessName, err := database.ContactNameGet(jid.User)
	if err == nil {
		if fullName != "" {
			name = fullName
		} else if businessName != "" {
			name = businessName + " [ " + jid.User + " ]"
		} else if pushName != "" {
			name = pushName + " [ " + jid.User + " ]"
		} else if firstName != "" {
			name = firstName + " [ " + jid.User + " ]"
		}
	} else {
		waClient := state.State.WhatsAppClient
		contact, err := waClient.Store.Contacts.GetContact(jid)
		if err == nil && contact.Found {
			if contact.FullName != "" {
				name = contact.FullName
			} else if contact.BusinessName != "" {
				name = contact.BusinessName + " [ " + jid.User + " ]"
			} else if contact.PushName != "" {
				name = contact.PushName + " [ " + jid.User + " ]"
			} else if contact.FirstName != "" {
				name = contact.FirstName + " [ " + jid.User + " ]"
			}
		}
	}

	if name == "" {
		name = jid.User
	}

	return name
}

func WaTagAll(group types.JID, msg *waProto.Message, msgId, msgSender string, msgIsFromMe bool) {
	var (
		cfg      = state.State.Config
		waClient = state.State.WhatsAppClient
		tgBot    = state.State.TelegramBot
	)

	groupInfo, err := waClient.GetGroupInfo(group)
	if err != nil {
		log.Printf("[whatsapp] failed to get group info of '%s': %s\n", group.String(), err)
		return
	}

	var (
		replyText = ""
		mentioned = []string{}
	)

	for _, participant := range groupInfo.Participants {
		if participant.JID.User == waClient.Store.ID.User {
			continue
		}

		replyText += fmt.Sprintf("@%s ", participant.JID.User)
		mentioned = append(mentioned, participant.JID.String())
	}

	_, err = waClient.SendMessage(context.Background(), group, &waProto.Message{
		ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(replyText),
			ContextInfo: &waProto.ContextInfo{
				StanzaId:      proto.String(msgId),
				Participant:   proto.String(msgSender),
				QuotedMessage: msg,
				MentionedJid:  mentioned,
			},
		},
	})
	if err != nil {
		log.Printf("[whatsapp] failed to reply to '@all/@everyone': %s\n", err)
		return
	}

	if !msgIsFromMe {
		tagsThreadId, err := TgGetOrMakeThreadFromWa("status@broadcast", cfg.Telegram.TargetChatID, "Status/Calls/Tags [ status@broadcast ]")
		if err != nil {
			TgSendErrorById(tgBot, cfg.Telegram.TargetChatID, 0, "Failed to create/retreive corresponding thread id for status/calls/tags", err)
			return
		}

		bridgedText := fmt.Sprintf("#tagall\n\nEveryone was mentioned in a group\n\n👥: <i>%s</i>",
			html.EscapeString(groupInfo.Name))

		TgSendTextById(tgBot, cfg.Telegram.TargetChatID, tagsThreadId, bridgedText)
	}
}

func WaSendText(chat types.JID, text, stanzaId, participantId string, quotedMsg *waProto.Message, isReply bool) (whatsmeow.SendResponse, error) {
	waClient := state.State.WhatsAppClient

	msgToSend := &waProto.Message{}
	if isReply {
		msgToSend.ExtendedTextMessage = &waProto.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waProto.ContextInfo{
				StanzaId:      proto.String(stanzaId),
				Participant:   proto.String(participantId),
				QuotedMessage: quotedMsg,
			},
		}
	} else {
		msgToSend.Conversation = proto.String(text)
	}

	return waClient.SendMessage(context.Background(), chat, msgToSend)
}
