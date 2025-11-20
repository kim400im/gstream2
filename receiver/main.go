// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/go-gst/go-gst/gst/app"
	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type Message struct {
	Type    string          `json:"type"`
	From    string          `json:"from"`
	To      string          `json:"to"`
	Payload json.RawMessage `json:"payload"`
	Role    string          `json:"role"`
}

var (
	ws   *websocket.Conn
	pc   *webrtc.PeerConnection
	myID string
)

func init() {
	runtime.LockOSThread()
}

func main() {
	// GStreamer 초기화
	gst.Init(nil)

	// 랜덤 ID 생성
	myID = fmt.Sprintf("receiver-%d", rand.Intn(10000))

	fmt.Printf("Starting receiver with ID: %s\n", myID)
	fmt.Println("Connecting to signaling server at ws://144.24.83.16:8080/ws")

	// WebSocket 연결
	var err error
	ws, _, err = websocket.DefaultDialer.Dial("ws://144.24.83.16:8080/ws", nil)
	if err != nil {
		panic(err)
	}
	defer ws.Close()

	// 서버에 등록
	registerMsg := Message{
		Type: "register",
		From: myID,
		Role: "receiver",
	}
	if err := ws.WriteJSON(registerMsg); err != nil {
		panic(err)
	}

	fmt.Println("Registered with signaling server")
	fmt.Println("Waiting for offer from sender...")

	// WebRTC PeerConnection 초기화
	initPeerConnection()

	// 메시지 수신 루프
	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			fmt.Println("WebSocket error:", err)
			break
		}

		handleSignalingMessage(msg)
	}
}

func initPeerConnection() {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	var err error
	pc, err = webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Track 수신 핸들러
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			// PLI 주기적으로 전송
			go func() {
				ticker := time.NewTicker(time.Second * 3)
				for range ticker.C {
					if rtcpErr := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); rtcpErr != nil {
						fmt.Println("RTCP error:", rtcpErr)
					}
				}
			}()
		}

		codecName := strings.Split(track.Codec().RTPCodecCapability.MimeType, "/")[1]
		fmt.Printf("Track received, type %d: %s\n", track.PayloadType(), codecName)

		// GStreamer 파이프라인 시작
		appSrc := pipelineForCodec(track, codecName)
		buf := make([]byte, 1400)
		for {
			i, _, readErr := track.Read(buf)
			if readErr != nil {
				fmt.Println("Track read error:", readErr)
				break
			}

			appSrc.PushBuffer(gst.NewBufferFromBytes(buf[:i]))
		}
	})

	// ICE 후보 핸들러
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		candidateJSON, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			fmt.Println("Error marshaling ICE candidate:", err)
			return
		}

		msg := Message{
			Type:    "ice-candidate",
			From:    myID,
			To:      "", // 송신자 ID는 offer를 받을 때 저장됨
			Payload: candidateJSON,
		}

		if err := ws.WriteJSON(msg); err != nil {
			fmt.Println("Error sending ICE candidate:", err)
		}
	})

	// ICE 연결 상태 핸들러
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State: %s\n", state.String())
	})

	fmt.Println("PeerConnection initialized")
}

func handleSignalingMessage(msg Message) {
	fmt.Printf("Received message type: %s from: %s\n", msg.Type, msg.From)

	switch msg.Type {
	case "registered":
		fmt.Println("Successfully registered with server")

	case "offer":
		handleOffer(msg)

	case "ice-candidate":
		handleICECandidate(msg)
	}
}

func handleOffer(msg Message) {
	fmt.Println("Received offer, creating answer...")

	var offer webrtc.SessionDescription
	if err := json.Unmarshal(msg.Payload, &offer); err != nil {
		fmt.Println("Error unmarshaling offer:", err)
		return
	}

	// Remote description 설정
	if err := pc.SetRemoteDescription(offer); err != nil {
		fmt.Println("Error setting remote description:", err)
		return
	}

	// Answer 생성
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		fmt.Println("Error creating answer:", err)
		return
	}

	// Local description 설정
	if err := pc.SetLocalDescription(answer); err != nil {
		fmt.Println("Error setting local description:", err)
		return
	}

	// Answer 전송
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		fmt.Println("Error marshaling answer:", err)
		return
	}

	answerMsg := Message{
		Type:    "answer",
		From:    myID,
		To:      msg.From,
		Payload: answerJSON,
	}

	if err := ws.WriteJSON(answerMsg); err != nil {
		fmt.Println("Error sending answer:", err)
		return
	}

	fmt.Println("Answer sent successfully")
}

func handleICECandidate(msg Message) {
	var candidate webrtc.ICECandidateInit
	if err := json.Unmarshal(msg.Payload, &candidate); err != nil {
		fmt.Println("Error unmarshaling ICE candidate:", err)
		return
	}

	if err := pc.AddICECandidate(candidate); err != nil {
		fmt.Println("Error adding ICE candidate:", err)
		return
	}

	fmt.Println("ICE candidate added")
}

func pipelineForCodec(track *webrtc.TrackRemote, codecName string) *app.Source {
	pipelineString := "appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp"

	switch strings.ToLower(codecName) {
	case "vp8":
		pipelineString += fmt.Sprintf(", payload=%d, encoding-name=VP8-DRAFT-IETF-01 ! rtpvp8depay ! decodebin ! autovideosink", track.PayloadType())
	case "opus":
		pipelineString += fmt.Sprintf(", payload=%d, encoding-name=OPUS ! rtpopusdepay ! decodebin ! autoaudiosink", track.PayloadType())
	case "vp9":
		pipelineString += " ! rtpvp9depay ! decodebin ! autovideosink"
	case "h264":
		pipelineString += " ! rtph264depay ! decodebin ! autovideosink"
	case "g722":
		pipelineString += " clock-rate=8000 ! rtpg722depay ! decodebin ! autoaudiosink"
	default:
		panic("Unhandled codec " + codecName)
	}

	pipeline, err := gst.NewPipelineFromString(pipelineString)
	if err != nil {
		panic(err)
	}

	if err = pipeline.SetState(gst.StatePlaying); err != nil {
		panic(err)
	}

	appSrc, err := pipeline.GetElementByName("src")
	if err != nil {
		panic(err)
	}

	return app.SrcFromElement(appSrc)
}
