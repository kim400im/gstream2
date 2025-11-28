// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 모든 origin 허용 (프로덕션에서는 제한 필요)
	},
}

type Client struct {
	conn   *websocket.Conn
	id     string
	role   string // "sender" or "receiver"
	roomID string // 참여 중인 방 ID
}

type Room struct {
	id      string
	clients map[string]*Client
}

type SignalingServer struct {
	rooms map[string]*Room // roomID -> Room
	mu    sync.Mutex
}

type Message struct {
	Type    string      `json:"type"`
	From    string      `json:"from"`
	To      string      `json:"to"`
	Payload interface{} `json:"payload"`
	Role    string      `json:"role"`
	RoomID  string      `json:"roomId,omitempty"`
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func NewSignalingServer() *SignalingServer {
	return &SignalingServer{
		rooms: make(map[string]*Room),
	}
}

// 랜덤 방 ID 생성
func generateRoomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 6)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}

func (s *SignalingServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	var client *Client

	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			log.Println("Read error:", err)
			if client != nil {
				s.removeClientFromRoom(client)
				log.Printf("Client %s (%s) disconnected\n", client.id, client.role)
			}
			break
		}

		switch msg.Type {
		case "register":
			// 클라이언트 등록 (방 입장 전)
			client = &Client{
				conn: conn,
				id:   msg.From,
				role: msg.Role,
			}

			log.Printf("Client registered: %s (role: %s)\n", client.id, client.role)

			// 등록 확인 응답
			response := Message{
				Type: "registered",
				From: "server",
				To:   client.id,
			}
			conn.WriteJSON(response)

		case "create-room":
			// 방 생성 (sender만)
			if client == nil {
				continue
			}

			roomID := generateRoomID()

			s.mu.Lock()
			s.rooms[roomID] = &Room{
				id:      roomID,
				clients: make(map[string]*Client),
			}
			// 방 생성자를 방에 추가
			s.rooms[roomID].clients[client.id] = client
			client.roomID = roomID
			s.mu.Unlock()

			log.Printf("Room created: %s by %s\n", roomID, client.id)

			// 방 생성 완료 응답
			response := Message{
				Type:    "room-created",
				From:    "server",
				To:      client.id,
				Payload: map[string]string{"roomId": roomID},
			}
			conn.WriteJSON(response)

		case "join-room":
			// 방 입장
			if client == nil {
				continue
			}

			roomID := msg.RoomID
			if roomID == "" {
				if payload, ok := msg.Payload.(map[string]interface{}); ok {
					if rid, ok := payload["roomId"].(string); ok {
						roomID = rid
					}
				}
			}

			s.mu.Lock()
			room, exists := s.rooms[roomID]
			if !exists {
				s.mu.Unlock()
				// 방이 없음
				response := Message{
					Type:    "error",
					From:    "server",
					To:      client.id,
					Payload: map[string]string{"message": "Room not found"},
				}
				conn.WriteJSON(response)
				continue
			}

			// 방에 클라이언트 추가
			room.clients[client.id] = client
			client.roomID = roomID
			s.mu.Unlock()

			log.Printf("Client %s joined room %s\n", client.id, roomID)

			// 방 입장 완료 응답
			response := Message{
				Type:    "room-joined",
				From:    "server",
				To:      client.id,
				Payload: map[string]string{"roomId": roomID},
			}
			conn.WriteJSON(response)

			// 같은 방의 클라이언트 목록 브로드캐스트
			s.broadcastClientListToRoom(roomID)

		case "offer", "answer", "ice-candidate":
			// 메시지를 목적지 클라이언트에게 전달
			if client == nil || client.roomID == "" {
				continue
			}

			s.mu.Lock()
			room, exists := s.rooms[client.roomID]
			if !exists {
				s.mu.Unlock()
				continue
			}

			targetClient, exists := room.clients[msg.To]
			s.mu.Unlock()

			if exists {
				err := targetClient.conn.WriteJSON(msg)
				if err != nil {
					log.Printf("Error sending to %s: %v\n", msg.To, err)
				} else {
					log.Printf("Forwarded %s from %s to %s in room %s\n", msg.Type, msg.From, msg.To, client.roomID)
				}
			} else {
				log.Printf("Target client %s not found in room %s\n", msg.To, client.roomID)
			}
		}
	}
}

func (s *SignalingServer) removeClientFromRoom(client *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client.roomID == "" {
		return
	}

	room, exists := s.rooms[client.roomID]
	if !exists {
		return
	}

	delete(room.clients, client.id)

	// 방에 아무도 없으면 방 삭제
	if len(room.clients) == 0 {
		delete(s.rooms, client.roomID)
		log.Printf("Room %s deleted (empty)\n", client.roomID)
	} else {
		// 남은 클라이언트들에게 목록 업데이트
		go s.broadcastClientListToRoom(client.roomID)
	}
}

func (s *SignalingServer) broadcastClientListToRoom(roomID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, exists := s.rooms[roomID]
	if !exists {
		return
	}

	clientList := make(map[string]string)
	for id, client := range room.clients {
		clientList[id] = client.role
	}

	msg := Message{
		Type:    "client-list",
		From:    "server",
		Payload: clientList,
		RoomID:  roomID,
	}

	for _, client := range room.clients {
		client.conn.WriteJSON(msg)
	}
}

func (s *SignalingServer) serveHTML(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "sender.html")
}

func main() {
	server := NewSignalingServer()

	http.HandleFunc("/ws", server.handleWebSocket)
	http.HandleFunc("/", server.serveHTML)

	fmt.Println("Signaling server started on :8080")
	fmt.Println("Sender: Open http://localhost:8080 in browser")
	fmt.Println("Receiver: Run the receiver Go program with room ID")

	log.Fatal(http.ListenAndServe(":8080", nil))
}
