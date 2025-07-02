package handlers

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"p2p_file_transfer/utils"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

type Offer struct {
	Index       int    `json:"index"`
	SDP         string `json:"sdp"`
	ChannelType string `json:"channelType"`
}
type SDPRequest struct {
	Offers       []Offer      `json:"offers"`
	FileMetadata FileMetadata `json:"fileMetadata"`
}

type SDPResponse struct {
	Answers      []Answer         `json:"answers"`
	FileMetadata MetadataResponse `json:"fileMetadata"`
}
type MetadataResponse struct {
	TotalChunks int64 `json:"totalChunks"`
	ChunkSize   int   `json:"chunkSize"`
}
type PreloadedFile struct {
	Packets     [][]byte
	TotalChunks int64
	ChunkSize   int
}
type Answer struct {
	Index int    `json:"index"`
	SDP   string `json:"sdp"`
}

type ControlMessage struct {
	Type       string `json:"type"`
	ChunkIndex uint64 `json:"chunkIndex"`
	ChannelId  int    `json:"channelId"`
}
type PreloadResult struct {
	File  *PreloadedFile
	Error error
}
type PendingChunk struct {
	ChunkIndex uint64
	Packet     []byte
	ChannelId  int
	RetryCount int
}

type ChunkTracker struct {
	timeBuckets   map[int64][]uint64
	pendingChunks map[uint64]*PendingChunk
	mu            sync.RWMutex
	retryInterval int64
	maxRetries    int
}

func NewChunkTracker() *ChunkTracker {
	return &ChunkTracker{
		timeBuckets:   make(map[int64][]uint64),
		pendingChunks: make(map[uint64]*PendingChunk),
		retryInterval: 2,
		maxRetries:    3,
	}
}

func (ct *ChunkTracker) AddPendingChunk(chunkIndex uint64, data []byte, channelId int) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	currentTime := time.Now().Unix()
	retryTime := currentTime + ct.retryInterval

	ct.timeBuckets[retryTime] = append(ct.timeBuckets[retryTime], chunkIndex)

	ct.pendingChunks[chunkIndex] = &PendingChunk{
		ChunkIndex: chunkIndex,
		Packet:     data,
		ChannelId:  channelId,
	}
}

func (ct *ChunkTracker) HandleACK(chunkIndex uint64) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	if _, exists := ct.pendingChunks[chunkIndex]; exists {
		delete(ct.pendingChunks, chunkIndex)
		fmt.Printf("ACK received for chunk %d\n", chunkIndex)
	}
}

func (ct *ChunkTracker) StartCleanupTimer(dataChannels []*webrtc.DataChannel) {
	ticker := time.NewTicker((1 * time.Second))

	go func() {
		defer ticker.Stop()
		for range ticker.C {
			currentTime := time.Now().Unix()
			ct.ProcessTimeouts(currentTime, dataChannels)
		}
	}()
}
func (ct *ChunkTracker) ProcessTimeouts(currentTime int64, dataChannels []*webrtc.DataChannel) {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Check if any chunks need to be retried at this time
	if chunksToRetry, exists := ct.timeBuckets[currentTime]; exists {
		fmt.Printf("Processing %d chunks for timeout at time %d\n", len(chunksToRetry), currentTime)

		for _, chunkIndex := range chunksToRetry {
			if pending, exists := ct.pendingChunks[chunkIndex]; exists {
				ct.HandleTimeout(pending, dataChannels)
			}
		}

		// Clean up processed time bucket
		delete(ct.timeBuckets, currentTime)
	}
}

func (ct *ChunkTracker) HandleTimeout(pending *PendingChunk, dataChannels []*webrtc.DataChannel) {
	// Check if channel is still open before attempting retry
	dc := dataChannels[pending.ChannelId]
	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		delete(ct.pendingChunks, pending.ChunkIndex)
		fmt.Printf("Channel %d closed, removing chunk %d from pending\n", pending.ChannelId, pending.ChunkIndex)
		return
	}

	pending.RetryCount++

	if pending.RetryCount > ct.maxRetries {
		// Max retries exceeded - remove completely
		delete(ct.pendingChunks, pending.ChunkIndex)
		fmt.Printf("Chunk %d failed after %d retries, giving up\n", pending.ChunkIndex, ct.maxRetries)
		return
	}

	fmt.Printf("Retrying chunk %d (attempt %d/%d)\n", pending.ChunkIndex, pending.RetryCount, ct.maxRetries)

	// Retransmit using existing function
	if err := sendPacketWithTracking(dc, pending.Packet, pending.ChunkIndex, ct, pending.ChannelId); err != nil {
		fmt.Printf("Failed to retransmit chunk %d: %v\n", pending.ChunkIndex, err)
		// Check if failure is due to closed channel
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			delete(ct.pendingChunks, pending.ChunkIndex)
			return
		}
		// Schedule another retry only if channel is still open
		nextRetryTime := time.Now().Unix() + ct.retryInterval
		ct.timeBuckets[nextRetryTime] = append(ct.timeBuckets[nextRetryTime], pending.ChunkIndex)
	} else {
		// Successfully retransmitted, tracking is handled by sendPacketWithTracking
		fmt.Printf("Retransmitted chunk %d successfully\n", pending.ChunkIndex)
	}
}

func preloadFile(filePath string, chunkSize int) (*PreloadedFile, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file:%v", err)
	}
	defer file.Close()

	var packets [][]byte
	buf := make([]byte, chunkSize)
	var chunkIdx uint64

	for {
		n, err := file.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to read file:%v", err)
		}
		if n == 0 {
			break
		}

		packet := make([]byte, 8+n)
		binary.BigEndian.PutUint64(packet[:8], chunkIdx)
		copy(packet[8:], buf[:n])
		packets = append(packets, packet)
		chunkIdx++
	}

	return &PreloadedFile{
		Packets:     packets,
		TotalChunks: int64(len(packets)),
		ChunkSize:   chunkSize,
	}, nil
}

func startFileSending(file *PreloadedFile, dataChannels []*webrtc.DataChannel, tracker *ChunkTracker) {
	go sendChunksForChannel(file, dataChannels[0], 0, tracker)
	go sendChunksForChannel(file, dataChannels[1], 1, tracker)
	go sendChunksForChannel(file, dataChannels[2], 2, tracker)
}

func sendChunksForChannel(file *PreloadedFile, dc *webrtc.DataChannel, offset int, tracker *ChunkTracker) {
	for i := offset; i < int(file.TotalChunks); i += 3 {
		chunkIndex := binary.BigEndian.Uint64(file.Packets[i][:8])
		if err := sendPacketWithTracking(dc, file.Packets[i], chunkIndex, tracker, offset); err != nil {
			fmt.Printf("Failed to send chunk %d, stopping channel %d\n", chunkIndex, offset)
			return
		}
	}
}

func sendPacketWithTracking(dc *webrtc.DataChannel, packet []byte, chunkIndex uint64, tracker *ChunkTracker, channelId int) error {
	// Check channel state before attempting send
	if dc.ReadyState() != webrtc.DataChannelStateOpen {
		return fmt.Errorf("channel %d is not open", channelId)
	}

	// Wait for buffer space
	for dc.BufferedAmount() > 16384*10 {
		time.Sleep((10 * time.Millisecond))
		// Re-check channel state during wait
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return fmt.Errorf("channel %d closed while waiting for buffer", channelId)
		}
	}

	if err := dc.Send(packet); err != nil {
		fmt.Printf("Failed to send chunk %d:%v\n", chunkIndex, err)
		// Don't add to tracker if send failed - no point retransmitting
		return err
	}

	// Only add to tracker if send was successful
	tracker.AddPendingChunk(chunkIndex, packet, channelId)
	fmt.Printf("Sent chunk %d on channel %d\n", chunkIndex, channelId)
	return nil
}
func SDPHandler(w http.ResponseWriter, r *http.Request) {
	handleError := func(pc *webrtc.PeerConnection, status int, msg string, err error) {
		fmt.Println("Error encountered: ", err.Error())
		if pc != nil {
			pc.Close()
		}
		utils.RespondWithError(w, status, msg)
	}

	var req SDPRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&req)
	if err != nil {
		handleError(nil, http.StatusBadRequest, "Invalid request payload", err)
		return
	}
	fmt.Println(req.Offers[0].ChannelType)
	filePath, err := url.QueryUnescape(req.FileMetadata.Path)
	if err != nil {
		handleError(nil, http.StatusBadRequest, "Invalid file path encoding", err)
		return
	}
	fileInfo, err := os.Stat(filePath)
	if err != nil || fileInfo.Size() != req.FileMetadata.Size {
		handleError(nil, http.StatusBadRequest, "File not found or size mismatch", err)
		return
	}

	preloadChan := make(chan PreloadResult, 1)
	const chunkSize = 16384
	var file *PreloadedFile
	go func() {
		file, err = preloadFile(filePath, chunkSize)
		preloadChan <- PreloadResult{File: file, Error: err}
	}()
	chunkTracker := NewChunkTracker()

	readyChan := make(chan struct{})
	var openChannels int32
	var dataChannels []*webrtc.DataChannel
	var controlChannel *webrtc.DataChannel
	var peerConnections []*webrtc.PeerConnection

	for _, offer := range req.Offers {
		config := webrtc.Configuration{}
		pc, err := webrtc.NewPeerConnection(config)
		if err != nil {
			handleError(nil, http.StatusInternalServerError, "Failed to create peer connection", err)
			return
		}
		peerConnections = append(peerConnections, pc)

		func(currentOffer Offer) {
			pc.OnDataChannel(func(dc *webrtc.DataChannel) {
				if currentOffer.ChannelType == "control" {
					controlChannel = dc
					controlChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
						var controlMsg ControlMessage
						if err := json.Unmarshal(msg.Data, &controlMsg); err != nil {
							fmt.Printf("Error parsing control message:%v\n", err)
							return
						}
						switch controlMsg.Type {
						case "ack":

							chunkTracker.HandleACK(controlMsg.ChunkIndex)
						case "ready":
							fmt.Println("Client is ready for transfer")
						default:
							fmt.Printf("Unknown control message type:%s\n", controlMsg.Type)
						}
					})
				} else {
					dataChannels = append(dataChannels, dc)
				}

				dc.OnOpen(func() {
					fmt.Printf("Channel opened - Index: %d, Type: %s\n",
						currentOffer.Index, currentOffer.ChannelType)
					count := atomic.AddInt32(&openChannels, 1)
					if count == 4 {
						close(readyChan)
					}
				})
			})
		}(offer)
	}

	type ConnectionResult struct {
		Index int
		SDP   string
		Error error
	}

	resultChan := make(chan ConnectionResult, len(req.Offers))
	var wg sync.WaitGroup

	for i, offer := range req.Offers {
		wg.Add(1)
		go func(index int, offer Offer, pc *webrtc.PeerConnection) {
			defer wg.Done()
			var iceWg sync.WaitGroup
			var iceCandidates []*webrtc.ICECandidate
			mu := sync.Mutex{}

			pc.OnICECandidate(func(c *webrtc.ICECandidate) {
				if c != nil {
					mu.Lock()
					iceCandidates = append(iceCandidates, c)
					mu.Unlock()
				} else {
					iceWg.Done()
				}
			})

			sdpOffer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  offer.SDP,
			}
			if err := pc.SetRemoteDescription(sdpOffer); err != nil {
				resultChan <- ConnectionResult{Index: index, Error: err}
				return
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				resultChan <- ConnectionResult{Index: index, Error: err}
				return
			}

			iceWg.Add(1)
			if err := pc.SetLocalDescription(answer); err != nil {
				resultChan <- ConnectionResult{Index: index, Error: err}
				return
			}

			done := make(chan struct{})
			go func() {
				iceWg.Wait()
				close(done)
			}()

			select {
			case <-done:
				fmt.Printf("ICE candidates gathered for connection %d\n", index)
				for _, c := range iceCandidates {
					if err := pc.AddICECandidate(c.ToJSON()); err != nil {
						resultChan <- ConnectionResult{Index: index, Error: err}
						return
					}
				}
				resultChan <- ConnectionResult{
					Index: index,
					SDP:   pc.LocalDescription().SDP,
					Error: nil,
				}

			case <-time.After(3 * time.Second):
				resultChan <- ConnectionResult{
					Index: index,
					Error: fmt.Errorf("ICE gathering timeout for index:%d", index),
				}
				return
			}

		}(i, offer, peerConnections[i])
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	var answers []Answer
	var hasError bool

	for result := range resultChan {
		if result.Error != nil {
			fmt.Printf("Error on conneciton %d : %v\n", result.Index, result.Error)
			hasError = true
			break
		}
		answers = append(answers, Answer{
			Index: result.Index,
			SDP:   result.SDP,
		})
	}
	if hasError {
		for _, pc := range peerConnections {
			pc.Close()
		}
		handleError(nil, http.StatusInternalServerError, "Failed to establish connection", fmt.Errorf("connection setup failed"))
		return
	}
	sort.Slice(answers, func(i, j int) bool {
		return answers[i].Index < answers[j].Index
	})
	resp := SDPResponse{
		Answers: answers,
		FileMetadata: MetadataResponse{
			ChunkSize:   chunkSize,
			TotalChunks: (req.FileMetadata.Size + chunkSize - 1) / chunkSize,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	utils.RespondWithJSON(w, http.StatusOK, resp)

	go func() {
		preloadResult := <-preloadChan
		if preloadResult.Error != nil {
			fmt.Printf("Preload failed:%v\n", preloadResult.Error)
			return
		}
		fmt.Println("File Preloaded")
		select {
		case <-readyChan:
			startFileSending(preloadResult.File, dataChannels, chunkTracker)
			chunkTracker.StartCleanupTimer(dataChannels)

		case <-time.After(10 * time.Second):
			fmt.Println("Channels not ready in time")
		}
	}()

}
