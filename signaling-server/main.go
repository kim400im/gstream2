// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 모든 origin 허용 (프로덕션에서는 제한 필요)
	},
}

type Client struct {
	conn *websocket.Conn
	id   string
	role string // "sender" or "receiver"
}

type SignalingServer struct {
	clients map[string]*Client
	mu      sync.Mutex
}

type Message struct {
	Type    string      `json:"type"` // "offer", "answer", "ice-candidate", "register"
	From    string      `json:"from"`
	To      string      `json:"to"`
	Payload interface{} `json:"payload"`
	Role    string      `json:"role"` // "sender" or "receiver"
}

func NewSignalingServer() *SignalingServer {
	return &SignalingServer{
		clients: make(map[string]*Client),
	}
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
				s.mu.Lock()
				delete(s.clients, client.id)
				s.mu.Unlock()
				log.Printf("Client %s (%s) disconnected\n", client.id, client.role)
			}
			break
		}

		switch msg.Type {
		case "register":
			// 클라이언트 등록
			clientID := msg.From
			role := msg.Role

			client = &Client{
				conn: conn,
				id:   clientID,
				role: role,
			}

			s.mu.Lock()
			s.clients[clientID] = client
			s.mu.Unlock()

			log.Printf("Client registered: %s (role: %s)\n", clientID, role)

			// 등록 확인 응답
			response := Message{
				Type: "registered",
				From: "server",
				To:   clientID,
			}
			conn.WriteJSON(response)

			// 현재 연결된 클라이언트 목록 전송
			s.broadcastClientList()

		case "offer", "answer", "ice-candidate":
			// 메시지를 목적지 클라이언트에게 전달
			s.mu.Lock()
			targetClient, exists := s.clients[msg.To]
			s.mu.Unlock()

			if exists {
				err := targetClient.conn.WriteJSON(msg)
				if err != nil {
					log.Printf("Error sending to %s: %v\n", msg.To, err)
				} else {
					log.Printf("Forwarded %s from %s to %s\n", msg.Type, msg.From, msg.To)
				}
			} else {
				log.Printf("Target client %s not found\n", msg.To)
			}
		}
	}
}

func (s *SignalingServer) broadcastClientList() {
	s.mu.Lock()
	defer s.mu.Unlock()

	clientList := make(map[string]string)
	for id, client := range s.clients {
		clientList[id] = client.role
	}

	msg := Message{
		Type:    "client-list",
		From:    "server",
		Payload: clientList,
	}

	for _, client := range s.clients {
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
	fmt.Println("Receiver: Run the receiver Go program")

	log.Fatal(http.ListenAndServe(":8080", nil))
}
