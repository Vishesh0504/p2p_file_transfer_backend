package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"p2p_file_transfer/utils"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

type SDPRequest struct {
    SDP      string `json:"sdp"`
    FileMetadata FileMetadata `json:"fileMetadata"`
}
type SDPResponse struct {
	Answer       string       `json:"answer"`
	FileMetadata MetadataResponse `json:"fileMetadata"`
}
type MetadataResponse struct{
	TotalChunks int64 `json:"totalChunks"`
	ChunkSize int `json:"chunkSize"`
}
func SDPHandler(w http.ResponseWriter, r *http.Request){
	handleError := func(pc *webrtc.PeerConnection,status int, msg string,err error){
		fmt.Println("Error encountered: ",err.Error())
		if pc!=nil{
			pc.Close()
		}
		utils.RespondWithError(w,status,msg)
	}

	var req SDPRequest
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(&req)
	if err!=nil{
		handleError(nil,http.StatusBadRequest,"Invalid request payload",err)
		return
	}

	filePath, err := url.QueryUnescape(req.FileMetadata.Path)
	if err != nil {
		handleError(nil, http.StatusBadRequest, "Invalid file path encoding", err)
		return
	}
	fileInfo,err := os.Stat(filePath)
	if err!=nil || fileInfo.Size() != req.FileMetadata.Size{
		handleError(nil,http.StatusBadRequest,"File not found or size mismatch",err)
		return
	}

	config := webrtc.Configuration{}
	pc,err := webrtc.NewPeerConnection(config)
	if err!=nil{
		handleError(nil,http.StatusInternalServerError,"Failed to create peer connection",err)
		return
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel){
		dc.OnOpen(func(){
			if err:= sendFile(dc,filePath);err!=nil{
				dc.Close()
				handleError(pc,http.StatusInternalServerError,"Failed to send file",err)
				return
			}
			dc.SendText(`{"type":"done"}`)


		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Received message: %s\n", string(msg.Data))

		})

	})
	var wg sync.WaitGroup
	var iceCandidates []*webrtc.ICECandidate
	mu :=sync.Mutex{}

	pc.OnICECandidate(func (c *webrtc.ICECandidate){
		if c!=nil{
			mu.Lock()
			iceCandidates =append(iceCandidates,c)
			mu.Unlock()
		}else{
			wg.Done()
		}
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:req.SDP,
	}
	if err:= pc.SetRemoteDescription(offer); err!=nil{
		handleError(pc,http.StatusInternalServerError,"Couldn't accept the remote offer",err)
		return
	}

	answer,err := pc.CreateAnswer(nil)
	if err!=nil{
		handleError(pc,http.StatusInternalServerError,"Failed to create the answer of the offer",err)
		return
	}

	wg.Add(1)
	if err:= pc.SetLocalDescription(answer); err!=nil{
		handleError(pc,http.StatusInternalServerError,"Failed to set local description",err)
		return
	}

	done := make(chan struct{})
	go func(){
		wg.Wait()
		close(done)
	}()

	select{
	case<-done:
		fmt.Println("ICE candidates gathered")
	case<-time.After(2*time.Second):
		handleError(pc,http.StatusInternalServerError,"ICE Candidates gathering timed out",err)
		return
	}

	for _,c := range(iceCandidates){
		if err:=pc.AddICECandidate(c.ToJSON());err!=nil{
			handleError(pc,http.StatusInternalServerError,"Error adding ice candidates",err);
			return
		}
	}

	// fmt.Println(pc.LocalDescription().SDP)
	const chunkSize = 16384
	resp := SDPResponse{
		Answer: pc.LocalDescription().SDP,
		FileMetadata: MetadataResponse{
			ChunkSize:   chunkSize,
			TotalChunks: (req.FileMetadata.Size + chunkSize - 1) / chunkSize,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	utils.RespondWithJSON(w,http.StatusOK,resp)


}

func sendFile(dc *webrtc.DataChannel,filePath string) error{
	file,err := os.Open(filePath)

	if err!=nil{
		return fmt.Errorf("failed to open file %v",err)
	}
	defer file.Close()
	const chunkSize = 16384
	buf:=make([]byte,chunkSize)
	var chunkIdx int64

	for{
		n,err:= file.Read(buf)
		if err!=nil {
			if  err == io.EOF{
				break
			}
			return fmt.Errorf("failed to read file %v",err)
		}
		if n==0{
			break
		}
		for(dc.BufferedAmount()>chunkSize*10){
			time.Sleep(10*time.Millisecond)
		}
		if err:=dc.Send(buf[:n]);err!=nil{
			return fmt.Errorf("failed to send file chunk %v",err)
		}
		chunkIdx++

	}

	return nil
}