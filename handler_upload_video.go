package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")

	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You can't upload a video for this video", err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<30) // 10MB
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	mimeType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid content type", err)
		return
	}

	if mimeType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid content type", err)
		return
	}

	tempFile, err := os.CreateTemp(os.TempDir(), "video-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to copy file", err)
	}

	tempFile.Seek(0, io.SeekStart)

	key := make([]byte, 32)
	_, err = rand.Read(key)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate random bytes", err)
		return
	}

	encodedKey := base64.RawURLEncoding.EncodeToString(key)

	fmt.Println("tempFile.Name()", fmt.Sprintf("%s", tempFile.Name()))
	aspectRatio, err := getVideoAspectRatio(fmt.Sprintf("%s", tempFile.Name()))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get video aspect ratio", err)
		return
	}
	fmt.Println("aspectRatio", aspectRatio)

	aspectRatioParts := strings.Split(aspectRatio, ":")
	width, _ := strconv.Atoi(aspectRatioParts[0])
	height, _ := strconv.Atoi(aspectRatioParts[1])

	var aspectRatioName string

	if width > height {
		aspectRatioName = "landscape"
	} else if width < height {
		aspectRatioName = "portrait"
	} else {
		aspectRatioName = "other"
	}

	processedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process video", err)
		return
	}
	defer os.Remove(processedPath)

	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to open processed video", err)
		return
	}
	defer processedFile.Close()

	keyString := fmt.Sprintf("%s/%s%s", aspectRatioName, encodedKey, ".mp4")
	_, err = cfg.s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(keyString),
		Body:        processedFile,
		ContentType: aws.String(mimeType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to upload file", err)
		return
	}

	video.VideoURL = aws.String(fmt.Sprintf("%s/%s", cfg.s3CfDistribution, keyString))

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}


func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error processing video: %w", err)
	}
	return outputPath, nil
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}

	var output map[string]interface{}
	err = json.Unmarshal(out.Bytes(), &output)
	if err != nil {
		log.Fatal(err)
	}

	var width, height int
	streams := output["streams"].([]interface{})
	stream := streams[0].(map[string]any)
	width = int(stream["width"].(float64))
	height = int(stream["height"].(float64))
	gcd := gcd(width, height)
	return fmt.Sprintf("%d:%d", width/gcd, height/gcd), nil
}

func gcd(a, b int) int {
	if b == 0 {
		return a
	}
	return gcd(b, a%b)
}
