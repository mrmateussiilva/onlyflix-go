package transcoder

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"onlyflix/catalog"
	"onlyflix/types"
)

type TranscodeStatus string

const (
	StatusPending    TranscodeStatus = "pending"
	StatusProcessing TranscodeStatus = "processing"
	StatusCompleted  TranscodeStatus = "completed"
	StatusFailed     TranscodeStatus = "failed"
)

type TranscodeJob struct {
	FileID     string          `json:"file_id"`
	FileName   string          `json:"file_name"`
	FilePath   string          `json:"file_path"`
	Status     TranscodeStatus `json:"status"`
	Progress   float64         `json:"progress"`
	Error      string          `json:"error,omitempty"`
	Duration   float64         `json:"duration"`
	VideoCodec string          `json:"video_codec"`
	AudioCodec string          `json:"audio_codec"`
	DestDir    string          `json:"dest_dir"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type FFProbeResult struct {
	Format *struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
	} `json:"streams"`
}

var (
	transcodeJobs      = make(map[string]*TranscodeJob)
	transcodeMutex     sync.RWMutex
	transcodeFile      = "transcode_status.json"
	transcodeQueueChan = make(chan string, 1000)
	TranscodeDir       string
)

func InitTranscoder() {
	TranscodeDir = os.Getenv("TRANSCODE_DIR")
	if TranscodeDir == "" {
		TranscodeDir = "transcoded"
	}

	if err := os.MkdirAll(TranscodeDir, 0755); err != nil {
		log.Printf("[TRANSCODE] Erro ao criar diretório de transcodificação: %v", err)
	}

	if err := loadTranscodeStatus(); err != nil {
		log.Printf("[TRANSCODE] Erro ao carregar status do transcoder: %v", err)
	}

	transcodeMutex.Lock()
	modified := false
	for _, job := range transcodeJobs {
		if job.Status == StatusProcessing {
			job.Status = StatusPending
			job.Progress = 0
			job.UpdatedAt = time.Now()
			modified = true
		}
	}
	if modified {
		saveTranscodeStatusNoLock()
	}
	transcodeMutex.Unlock()
}

func loadTranscodeStatus() error {
	transcodeMutex.Lock()
	defer transcodeMutex.Unlock()

	if _, err := os.Stat(transcodeFile); os.IsNotExist(err) {
		transcodeJobs = make(map[string]*TranscodeJob)
		return nil
	}

	b, err := os.ReadFile(transcodeFile)
	if err != nil {
		return err
	}

	return json.Unmarshal(b, &transcodeJobs)
}

func saveTranscodeStatusNoLock() error {
	b, err := json.MarshalIndent(transcodeJobs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(transcodeFile, b, 0644)
}

func SaveTranscodeStatus() {
	transcodeMutex.Lock()
	defer transcodeMutex.Unlock()
	saveTranscodeStatusNoLock()
}

func HashString(s string) string {
	h := sha256.New()
	h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func EnqueueNewCatalogVideos(cat *types.SyncCatalog) {
	if cat == nil {
		return
	}

	transcodeMutex.Lock()
	defer transcodeMutex.Unlock()

	var addedAny bool

	checkAndAdd := func(v types.SyncVideo) {
		if _, exists := transcodeJobs[v.Id]; !exists {
			absPath, err := catalog.ResolvePath(catalog.LocalRoot, v.Id)
			if err != nil {
				return
			}
			job := &TranscodeJob{
				FileID:    v.Id,
				FileName:  v.Name,
				FilePath:  absPath,
				Status:    StatusPending,
				Progress:  0.0,
				UpdatedAt: time.Now(),
			}
			transcodeJobs[v.Id] = job
			addedAny = true

			select {
			case transcodeQueueChan <- v.Id:
			default:
				log.Printf("[TRANSCODE] Fila cheia, ignorando temporariamente: %s", v.Id)
			}
		}
	}

	for _, v := range cat.RootVideos {
		checkAndAdd(v)
	}

	for _, f := range cat.Folders {
		for _, v := range f.Videos {
			checkAndAdd(v)
		}
	}

	if addedAny {
		saveTranscodeStatusNoLock()
	}
}

func StartTranscoderWorker() {
	log.Println("[TRANSCODE] Iniciando background transcoder worker...")

	transcodeMutex.RLock()
	for id, job := range transcodeJobs {
		if job.Status == StatusPending {
			select {
			case transcodeQueueChan <- id:
			default:
			}
		}
	}
	transcodeMutex.RUnlock()

	for fileID := range transcodeQueueChan {
		processTranscodeJob(fileID)
	}
}

func updateJobStatus(fileID string, status TranscodeStatus, progress float64, errStr string) {
	transcodeMutex.Lock()
	defer transcodeMutex.Unlock()

	if job, ok := transcodeJobs[fileID]; ok {
		job.Status = status
		job.Progress = progress
		job.Error = errStr
		job.UpdatedAt = time.Now()
		saveTranscodeStatusNoLock()
	}
}

func updateJobProgress(fileID string, progress float64) {
	transcodeMutex.Lock()
	defer transcodeMutex.Unlock()

	if job, ok := transcodeJobs[fileID]; ok {
		job.Progress = float64(int(progress*10)) / 10.0
		job.UpdatedAt = time.Now()
	}
}

func processTranscodeJob(fileID string) {
	transcodeMutex.Lock()
	job, exists := transcodeJobs[fileID]
	transcodeMutex.Unlock()

	if !exists {
		return
	}

	log.Printf("[TRANSCODE] Iniciando processamento de: %s", job.FileName)
	updateJobStatus(fileID, StatusProcessing, 0.0, "")

	probeResult, err := probeVideo(job.FilePath)
	if err != nil {
		log.Printf("[TRANSCODE] Erro ao analisar mídia %s: %v", job.FileName, err)
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("ffprobe error: %v", err))
		return
	}

	duration, err := strconv.ParseFloat(probeResult.Format.Duration, 64)
	if err != nil {
		duration = 0
	}

	var vCodec, aCodec string
	for _, stream := range probeResult.Streams {
		if stream.CodecType == "video" && vCodec == "" {
			vCodec = stream.CodecName
		}
		if stream.CodecType == "audio" && aCodec == "" {
			aCodec = stream.CodecName
		}
	}

	transcodeMutex.Lock()
	job.Duration = duration
	job.VideoCodec = vCodec
	job.AudioCodec = aCodec
	destFolderName := HashString(fileID)
	job.DestDir = filepath.Join(TranscodeDir, destFolderName)
	transcodeMutex.Unlock()

	if err := os.MkdirAll(job.DestDir, 0755); err != nil {
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("mkdir error: %v", err))
		return
	}

	vFlag := []string{"-c:v", "libx264", "-preset", "fast", "-crf", "23"}
	if vCodec == "h264" {
		vFlag = []string{"-c:v", "copy"}
		log.Printf("[TRANSCODE] Vídeo %s já está em H264. Usando copy para o stream de vídeo.", job.FileName)
	}

	aFlag := []string{"-c:a", "aac", "-b:a", "128k"}
	if aCodec == "aac" {
		aFlag = []string{"-c:a", "copy"}
		log.Printf("[TRANSCODE] Áudio %s já está em AAC. Usando copy para o stream de áudio.", job.FileName)
	}

	inputPath, err := filepath.Abs(job.FilePath)
	if err != nil {
		inputPath = job.FilePath
	}

	args := []string{"-progress", "-", "-y", "-i", inputPath}
	args = append(args, vFlag...)
	args = append(args, aFlag...)
	args = append(args, []string{
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_segment_filename", "segment_%03d.ts",
		"index.m3u8",
	}...)

	cmd := exec.Command("ffmpeg", args...)
	cmd.Dir = job.DestDir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("stdout pipe error: %v", err))
		return
	}

	if err := cmd.Start(); err != nil {
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("ffmpeg start error: %v", err))
		return
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_us=") {
			usStr := strings.TrimPrefix(line, "out_time_us=")
			us, err := strconv.ParseInt(usStr, 10, 64)
			if err == nil && duration > 0 {
				elapsedSeconds := float64(us) / 1000000.0
				progress := (elapsedSeconds / duration) * 100.0
				if progress > 99.9 {
					progress = 99.9
				}
				if progress < 0 {
					progress = 0
				}
				updateJobProgress(fileID, progress)
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("[TRANSCODE] Falha ao processar vídeo %s: %v", job.FileName, err)
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("ffmpeg run error: %v", err))
		return
	}

	log.Printf("[TRANSCODE] Concluído processamento HLS para: %s", job.FileName)
	updateJobStatus(fileID, StatusCompleted, 100.0, "")
}

func probeVideo(filePath string) (*FFProbeResult, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-show_entries", "stream=codec_type,codec_name",
		"-of", "json",
		filePath,
	)

	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var res FFProbeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}

	if res.Format == nil {
		return nil, fmt.Errorf("dados de formato inválidos no ffprobe")
	}

	return &res, nil
}

func GetTranscodeStatusList() []*TranscodeJob {
	transcodeMutex.RLock()
	defer transcodeMutex.RUnlock()

	list := make([]*TranscodeJob, 0, len(transcodeJobs))
	for _, job := range transcodeJobs {
		list = append(list, job)
	}
	return list
}

func RetryFailedJob(fileID string) error {
	transcodeMutex.Lock()
	job, exists := transcodeJobs[fileID]
	transcodeMutex.Unlock()

	if !exists {
		return fmt.Errorf("job não encontrado")
	}

	if job.Status == StatusFailed {
		updateJobStatus(fileID, StatusPending, 0.0, "")
		select {
		case transcodeQueueChan <- fileID:
		default:
			return fmt.Errorf("fila cheia")
		}
	}
	return nil
}

func IsTranscodeCompleted(fileID string) bool {
	transcodeMutex.RLock()
	job, ok := transcodeJobs[fileID]
	if !ok || job.Status != StatusCompleted {
		transcodeMutex.RUnlock()
		return false
	}
	destDir := job.DestDir
	transcodeMutex.RUnlock()

	if destDir == "" {
		destDir = filepath.Join(TranscodeDir, HashString(fileID))
	}
	if _, err := os.Stat(filepath.Join(destDir, "index.m3u8")); err != nil {
		return false
	}
	return true
}

func GetVideoDuration(fileID string) float64 {
	transcodeMutex.RLock()
	defer transcodeMutex.RUnlock()
	if job, ok := transcodeJobs[fileID]; ok && job.Duration > 0 {
		return job.Duration
	}
	return 0
}

func GetFileIDFromHash(hash string) string {
	transcodeMutex.RLock()
	defer transcodeMutex.RUnlock()
	for id := range transcodeJobs {
		if HashString(id) == hash {
			return id
		}
	}
	return ""
}
