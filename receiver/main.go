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
	"sync"
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

// VideoStats 비디오 통계 정보
type VideoStats struct {
	sync.Mutex
	codecName       string
	packetsReceived uint64
	bytesReceived   uint64
	framesReceived  uint64
	packetsLost     uint64
	startTime       time.Time
	lastStatsTime   time.Time
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
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codecName := strings.Split(track.Codec().RTPCodecCapability.MimeType, "/")[1]
		fmt.Printf("Track received, type %d: %s\n", track.PayloadType(), codecName)

		// 통계 초기화
		stats := &VideoStats{
			codecName:     codecName,
			startTime:     time.Now(),
			lastStatsTime: time.Now(),
		}

		if track.Kind() == webrtc.RTPCodecTypeVideo {
			// PLI 주기적으로 전송
			go func() {
				ticker := time.NewTicker(time.Second * 3)
				defer ticker.Stop()
				for range ticker.C {
					if rtcpErr := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); rtcpErr != nil {
						fmt.Println("RTCP error:", rtcpErr)
					}
				}
			}()

			// RTCP 리시버 리포트 읽기
			go func() {
				rtcpBuf := make([]byte, 1500)
				for {
					if _, _, rtcpErr := receiver.Read(rtcpBuf); rtcpErr != nil {
						return
					}
					// RTCP 패킷 처리는 자동으로 WebRTC 내부에서 처리됨
				}
			}()

			// 통계 출력 시작
			go printStats(stats, receiver)
		}

		// GStreamer 파이프라인 시작
		appSrc := pipelineForCodec(track, codecName)
		buf := make([]byte, 1400)
		for {
			i, _, readErr := track.Read(buf)
			if readErr != nil {
				fmt.Println("Track read error:", readErr)
				break
			}

			// 통계 업데이트
			stats.Lock()
			stats.packetsReceived++
			stats.bytesReceived += uint64(i)
			stats.framesReceived++
			stats.Unlock()

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

// printStats 주기적으로 화질 통계 출력
func printStats(stats *VideoStats, receiver *webrtc.RTPReceiver) {
	ticker := time.NewTicker(time.Second * 2)
	defer ticker.Stop()

	for range ticker.C {
		stats.Lock()

		elapsed := time.Since(stats.startTime).Seconds()
		if elapsed == 0 {
			stats.Unlock()
			continue
		}

		// 프레임레이트 계산 (FPS)
		fps := float64(stats.framesReceived) / elapsed

		// 비트레이트 계산 (kbps)
		bitrate := (float64(stats.bytesReceived) * 8) / elapsed / 1000

		// 패킷 손실률 계산
		totalPackets := stats.packetsReceived + stats.packetsLost
		var packetLossRate float64
		if totalPackets > 0 {
			packetLossRate = (float64(stats.packetsLost) / float64(totalPackets)) * 100
		}

		fmt.Println("\n========== 화질 정보 ==========")
		fmt.Printf("코덱: %s\n", stats.codecName)
		fmt.Printf("프레임레이트: %.2f FPS\n", fps)
		fmt.Printf("비트레이트: %.2f kbps\n", bitrate)
		fmt.Printf("수신 패킷: %d\n", stats.packetsReceived)
		fmt.Printf("수신 바이트: %d bytes (%.2f MB)\n", stats.bytesReceived, float64(stats.bytesReceived)/(1024*1024))
		fmt.Printf("패킷 손실: %d (%.2f%%)\n", stats.packetsLost, packetLossRate)
		fmt.Printf("총 프레임: %d\n", stats.framesReceived)
		fmt.Printf("실행 시간: %.2f초\n", elapsed)

		// RTCP Stats 가져오기
		if receiver != nil {
			track := receiver.Track()
			if track != nil {
				fmt.Printf("SSRC: %d\n", track.SSRC())
				fmt.Printf("PayloadType: %d\n", track.PayloadType())
			}
		}

		fmt.Println("==============================\n")

		stats.Unlock()
	}
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
