// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

//go:build !js
// +build !js

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
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
	RoomID  string          `json:"roomId,omitempty"`
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

// MJPEG 스트리밍을 위한 프레임 저장소
type FrameStore struct {
	sync.RWMutex
	frame []byte
}

var (
	ws         *websocket.Conn
	pc         *webrtc.PeerConnection
	myID       string
	roomID     string
	frameStore = &FrameStore{}
)

func init() {
	runtime.LockOSThread()
}

func main() {
	// 커맨드라인 플래그
	flag.StringVar(&roomID, "room", "", "Room ID to join (required)")
	flag.Parse()

	if roomID == "" {
		fmt.Println("Usage: ./receiver -room <room_id>")
		fmt.Println("Example: ./receiver -room abc123")
		return
	}

	// GStreamer 초기화
	gst.Init(nil)

	// 랜덤 ID 생성
	myID = fmt.Sprintf("receiver-%d", rand.Intn(10000))

	fmt.Printf("Starting receiver with ID: %s\n", myID)
	fmt.Printf("Joining room: %s\n", roomID)
	fmt.Println("Connecting to signaling server at ws://144.24.83.16:8080/ws")

	// HTTP 서버 시작 (MJPEG 스트리밍용)
	go startHTTPServer()

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

// HTTP 서버 시작
func startHTTPServer() {
	http.HandleFunc("/", handleViewer)
	http.HandleFunc("/stream", handleStream)

	fmt.Println("HTTP server started on :9000")
	fmt.Println("Open http://localhost:9000 to view the stream")

	if err := http.ListenAndServe(":9000", nil); err != nil {
		fmt.Println("HTTP server error:", err)
	}
}

// 뷰어 HTML 페이지
func handleViewer(w http.ResponseWriter, r *http.Request) {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>Receiver Viewer</title>
    <style>
        body {
            font-family: Arial, sans-serif;
            display: flex;
            flex-direction: column;
            align-items: center;
            padding: 20px;
            background: #1a1a1a;
            color: white;
        }
        h1 { margin-bottom: 20px; }
        #stream {
            max-width: 100%%;
            border: 2px solid #444;
            border-radius: 8px;
        }
        .status {
            margin-top: 10px;
            padding: 10px;
            background: #333;
            border-radius: 4px;
        }
        .room-info {
            background: #2a5298;
            padding: 10px 20px;
            border-radius: 4px;
            margin-bottom: 20px;
        }
    </style>
</head>
<body>
    <h1>Receiver Stream Viewer</h1>
    <div class="room-info">Room: %s</div>
    <img id="stream" src="/stream" alt="Stream" />
    <div class="status">
        Streaming from GStreamer receiver
    </div>
</body>
</html>`, roomID)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

// MJPEG 스트림 핸들러
func handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for {
		frameStore.RLock()
		frame := frameStore.frame
		frameStore.RUnlock()

		if len(frame) > 0 {
			fmt.Fprintf(w, "--frame\r\n")
			fmt.Fprintf(w, "Content-Type: image/jpeg\r\n")
			fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(frame))
			w.Write(frame)
			fmt.Fprintf(w, "\r\n")

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		time.Sleep(33 * time.Millisecond) // ~30fps
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
				}
			}()

			// 통계 출력 시작
			go printStats(stats, receiver)
		}

		// GStreamer 파이프라인 시작
		appSrc, appSink := pipelineForCodec(track, codecName)

		// appsink에서 JPEG 프레임 읽기 (비디오인 경우)
		if track.Kind() == webrtc.RTPCodecTypeVideo && appSink != nil {
			go func() {
				for {
					sample := appSink.PullSample()
					if sample == nil {
						continue
					}

					buffer := sample.GetBuffer()
					if buffer == nil {
						continue
					}

					samples := buffer.Map(gst.MapRead)
					if samples != nil {
						frameStore.Lock()
						frameStore.frame = make([]byte, len(samples.Bytes()))
						copy(frameStore.frame, samples.Bytes())
						frameStore.Unlock()
						buffer.Unmap()
					}
				}
			}()
		}

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
		// 등록 완료 후 방에 입장
		joinRoom()

	case "room-joined":
		fmt.Printf("Successfully joined room: %s\n", roomID)
		fmt.Println("Waiting for offer from sender...")

	case "error":
		var payload map[string]string
		json.Unmarshal(msg.Payload, &payload)
		fmt.Printf("Error: %s\n", payload["message"])

	case "client-list":
		fmt.Println("Received updated client list")

	case "offer":
		handleOffer(msg)

	case "ice-candidate":
		handleICECandidate(msg)
	}
}

func joinRoom() {
	fmt.Printf("Joining room: %s\n", roomID)

	payload, _ := json.Marshal(map[string]string{"roomId": roomID})

	joinMsg := Message{
		Type:    "join-room",
		From:    myID,
		RoomID:  roomID,
		Payload: payload,
	}

	if err := ws.WriteJSON(joinMsg); err != nil {
		fmt.Println("Error joining room:", err)
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
		fmt.Printf("방: %s\n", roomID)
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

		fmt.Println("==============================")

		stats.Unlock()
	}
}

func pipelineForCodec(track *webrtc.TrackRemote, codecName string) (*app.Source, *app.Sink) {
	var pipelineString string
	var hasVideoSink bool

	switch strings.ToLower(codecName) {
	case "vp8":
		// 비디오: JPEG 인코딩 후 appsink로 출력
		pipelineString = fmt.Sprintf(
			"appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp, payload=%d, encoding-name=VP8-DRAFT-IETF-01 ! rtpvp8depay ! decodebin ! videoconvert ! video/x-raw,format=RGB ! jpegenc quality=85 ! appsink name=sink emit-signals=true sync=false",
			track.PayloadType(),
		)
		hasVideoSink = true
	case "opus":
		pipelineString = fmt.Sprintf(
			"appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp, payload=%d, encoding-name=OPUS ! rtpopusdepay ! decodebin ! autoaudiosink",
			track.PayloadType(),
		)
		hasVideoSink = false
	case "vp9":
		pipelineString = "appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp ! rtpvp9depay ! decodebin ! videoconvert ! video/x-raw,format=RGB ! jpegenc quality=85 ! appsink name=sink emit-signals=true sync=false"
		hasVideoSink = true
	case "h264":
		pipelineString = "appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp ! rtph264depay ! decodebin ! videoconvert ! video/x-raw,format=RGB ! jpegenc quality=85 ! appsink name=sink emit-signals=true sync=false"
		hasVideoSink = true
	case "g722":
		pipelineString = "appsrc format=time is-live=true do-timestamp=true name=src ! application/x-rtp, clock-rate=8000 ! rtpg722depay ! decodebin ! autoaudiosink"
		hasVideoSink = false
	default:
		panic("Unhandled codec " + codecName)
	}

	fmt.Printf("GStreamer pipeline: %s\n", pipelineString)

	pipeline, err := gst.NewPipelineFromString(pipelineString)
	if err != nil {
		panic(err)
	}

	if err = pipeline.SetState(gst.StatePlaying); err != nil {
		panic(err)
	}

	appSrcElem, err := pipeline.GetElementByName("src")
	if err != nil {
		panic(err)
	}

	var appSink *app.Sink
	if hasVideoSink {
		appSinkElem, err := pipeline.GetElementByName("sink")
		if err != nil {
			panic(err)
		}
		appSink = app.SinkFromElement(appSinkElem)
	}

	return app.SrcFromElement(appSrcElem), appSink
}
