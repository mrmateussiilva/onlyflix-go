package transcoder

import (
	"bufio"
	"context"
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
	"onlyflix/database"
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
	transcodeQueueChan = make(chan string, 1000)
	TranscodeDir       string
)

func getEnvInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultVal
	}
	return n
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return defaultVal
	}
	return time.Duration(n) * time.Second
}

func checkFFmpeg() error {
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s não encontrado no PATH", name)
		}
	}
	return nil
}

func InitTranscoder() {
	TranscodeDir = os.Getenv("TRANSCODE_DIR")
	if TranscodeDir == "" {
		TranscodeDir = "transcoded"
	}

	if err := os.MkdirAll(TranscodeDir, 0755); err != nil {
		log.Printf("[TRANSCODE] Erro ao criar diretório de transcodificação: %v", err)
	}

	if err := checkFFmpeg(); err != nil {
		log.Printf("[TRANSCODE] AVISO: %v. Transcódificação HLS não funcionará.", err)
	} else {
		log.Println("[TRANSCODE] FFmpeg/FFprobe encontrados.")
	}

	loadJobsFromDB()

	transcodeMutex.Lock()
	for _, job := range transcodeJobs {
		if job.Status == StatusProcessing {
			job.Status = StatusPending
			job.Progress = 0
			job.UpdatedAt = time.Now()
		}
	}
	transcodeMutex.Unlock()
}

func loadJobsFromDB() {
	rows, err := database.DB.Query("SELECT file_id, file_name, file_path, status, progress, COALESCE(error,''), duration, COALESCE(video_codec,''), COALESCE(audio_codec,''), COALESCE(dest_dir,''), updated_at FROM transcode_jobs")
	if err != nil {
		log.Printf("[TRANSCODE] Erro ao carregar jobs do DB: %v", err)
		return
	}
	defer rows.Close()

	var dropped int
	tmp := make(map[string]*TranscodeJob)
	for rows.Next() {
		var job TranscodeJob
		var updatedStr string
		err := rows.Scan(&job.FileID, &job.FileName, &job.FilePath, &job.Status, &job.Progress, &job.Error, &job.Duration, &job.VideoCodec, &job.AudioCodec, &job.DestDir, &updatedStr)
		if err != nil {
			continue
		}
		job.UpdatedAt, _ = time.Parse(time.RFC3339, updatedStr)

		if _, err := os.Stat(job.FilePath); err != nil {
			dropped++
			continue
		}

		tmp[job.FileID] = &job
	}

	transcodeMutex.Lock()
	transcodeJobs = tmp
	transcodeMutex.Unlock()

	if dropped > 0 {
		log.Printf("[TRANSCODE] Jobs descartados do DB (arquivo inexistente): %d", dropped)
	}
}

func saveJobToDB(job *TranscodeJob) {
	_, err := database.DB.Exec(`INSERT INTO transcode_jobs (file_id, file_name, file_path, status, progress, error, duration, video_codec, audio_codec, dest_dir, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_id) DO UPDATE SET
			status=excluded.status, progress=excluded.progress, error=excluded.error,
			duration=excluded.duration, video_codec=excluded.video_codec, audio_codec=excluded.audio_codec,
			dest_dir=excluded.dest_dir, updated_at=excluded.updated_at`,
		job.FileID, job.FileName, job.FilePath, job.Status, job.Progress, job.Error,
		job.Duration, job.VideoCodec, job.AudioCodec, job.DestDir,
		job.UpdatedAt.Format(time.RFC3339))
	if err != nil {
		log.Printf("[TRANSCODE] Erro ao salvar job %s: %v", job.FileID, err)
	}
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
			saveJobToDB(job)

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
}

func StartTranscoderWorker() {
	maxWorkers := getEnvInt("TRANSCODE_MAX_WORKERS", 2)
	timeout := getEnvDuration("TRANSCODE_TIMEOUT", 3600)

	log.Printf("[TRANSCODE] Iniciando %d workers de transcodificação (timeout: %v)...", maxWorkers, timeout)

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

	for i := range maxWorkers {
		go func(workerID int) {
			log.Printf("[TRANSCODE] Worker %d iniciado.", workerID)
			for fileID := range transcodeQueueChan {
				processTranscodeJob(fileID, workerID, timeout)
			}
		}(i)
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
		saveJobToDB(job)
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

func processTranscodeJob(fileID string, workerID int, timeout time.Duration) {
	transcodeMutex.Lock()
	job, exists := transcodeJobs[fileID]
	transcodeMutex.Unlock()

	if !exists {
		return
	}

	log.Printf("[TRANSCODE] [Worker %d] Iniciando: %s", workerID, job.FileName)
	updateJobStatus(fileID, StatusProcessing, 0.0, "")

	probeResult, err := probeVideo(job.FilePath)
	if err != nil {
		log.Printf("[TRANSCODE] [Worker %d] Erro ffprobe %s: %v", workerID, job.FileName, err)
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
		log.Printf("[TRANSCODE] [Worker %d] Vídeo %s já em H264, copiando.", workerID, job.FileName)
	}

	aFlag := []string{"-c:a", "aac", "-b:a", "128k"}
	if aCodec == "aac" {
		aFlag = []string{"-c:a", "copy"}
		log.Printf("[TRANSCODE] [Worker %d] Áudio %s já em AAC, copiando.", workerID, job.FileName)
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

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if _, err := os.Stat(job.FilePath); err != nil {
		log.Printf("[TRANSCODE] [Worker %d] Arquivo nao encontrado %s: %v", workerID, job.FilePath, err)
		updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("arquivo nao encontrado: %v", err))
		return
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Dir = job.DestDir

	var ffmpegStderr strings.Builder
	cmd.Stderr = &ffmpegStderr

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
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("[TRANSCODE] [Worker %d] Timeout ao processar %s (%v)", workerID, job.FileName, timeout)
			updateJobStatus(fileID, StatusFailed, 0.0, fmt.Sprintf("timeout excedido (%v)", timeout))
		} else {
			errMsg := fmt.Sprintf("ffmpeg error: %v\nstderr: %s", err, ffmpegStderr.String())
			log.Printf("[TRANSCODE] [Worker %d] Falha ao processar %s: %s", workerID, job.FileName, errMsg)
			updateJobStatus(fileID, StatusFailed, 0.0, errMsg)
		}
		return
	}

	log.Printf("[TRANSCODE] [Worker %d] Concluido: %s", workerID, job.FileName)
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

	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe falhou para %s: %w\nstderr: %s", filePath, err, stderrBuf.String())
	}

	var res FFProbeResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("erro ao parsear saida ffprobe: %w\nsaida: %s", err, string(out))
	}

	if res.Format == nil {
		return nil, fmt.Errorf("ffprobe nao retornou dados de formato para: %s\nstderr: %s", filePath, stderrBuf.String())
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

func CleanupOrphanHLS() {
	entries, err := os.ReadDir(TranscodeDir)
	if err != nil {
		log.Printf("[TRANSCODE] Erro ao ler diretório para limpeza: %v", err)
		return
	}

	transcodeMutex.RLock()
	validHashes := make(map[string]bool, len(transcodeJobs))
	for id, job := range transcodeJobs {
		if job.Status == StatusCompleted {
			validHashes[HashString(id)] = true
		}
	}
	transcodeMutex.RUnlock()

	var removed int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !validHashes[e.Name()] {
			path := filepath.Join(TranscodeDir, e.Name())
			if err := os.RemoveAll(path); err != nil {
				log.Printf("[TRANSCODE] Erro ao remover HLS órfão %s: %v", e.Name(), err)
			} else {
				log.Printf("[TRANSCODE] Removido HLS órfão: %s", e.Name())
				removed++
			}
		}
	}
	if removed > 0 {
		log.Printf("[TRANSCODE] Limpeza concluída: %d pasta(s) removida(s).", removed)
	}
}
