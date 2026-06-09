package media

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"onlyflix/types"
)

func isVideoExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".ts", ".mpeg", ".mpg", ".3gp":
		return true
	}
	return false
}

func IsVideoExt(name string) bool {
	return isVideoExt(name)
}

func IsImageExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg":
		return true
	}
	return false
}

func MimeForFile(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts":
		return "video/mp2t"
	case ".m4v":
		return "video/x-m4v"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	default:
		return "video/mp4"
	}
}

func TagForExt(name string) (class, label string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".m4v", ".ts", ".mpeg", ".mpg", ".3gp":
		return "video", "Vídeo"
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg":
		return "image", "Imagem"
	case ".pdf":
		return "pdf", "PDF"
	default:
		return "", "Arquivo"
	}
}

func FormatSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/1024/1024)
}

var noisePatternsPre = []string{
	`(?i)\b(1080p|720p|2160p|480p|4k)\b`,
	`(?i)\b(amzn|web[.-]dl|dsnp|nf|pmtp|hmax|hbo|max)\b`,
	`(?i)\b(webrip|bluray|hdrip|bdrip|hdtv|dvdrip)\b`,
	`(?i)\b(h\.?264|x264|h\.?265|x265|hevc|avc|av1)\b`,
	`(?i)\b(ddp5\.?1|dd5\.?1|dd2\.?0|aac|ac3|eac3|dts|mp3|opus)\b`,
	`(?i)\b(atmos|hdr10?|hdr|dolby|vision|sdr|hlg)\b`,
	`(?i)\b(dual|dublado|legendado|multi)\b`,
	`(?i)\b(1080|720|2160|480)p\b`,
	`(?i)\b(complete|proper|repack|internal|readnfo)\b`,
}

var trailGroupPre = regexp.MustCompile(`(?i)[\s.-]+-\s+(SiGLA|FLUX|PiA|C76|RMB|FGT|NTb|PD_Dinho|SMURF)\s*$`)
var trailGroupPost = regexp.MustCompile(`(?i)\s+(SiGLA|FLUX|PiA|C76|RMB|FGT|NTb|PD_Dinho|SMURF)\s*$`)

func stripNoise(name string) string {
	for _, pat := range noisePatternsPre {
		name = regexp.MustCompile(pat).ReplaceAllString(name, "")
	}
	name = trailGroupPre.ReplaceAllString(name, "")
	return name
}

func CleanName(name string, stripExt bool) string {
	if stripExt {
		if idx := strings.LastIndex(name, "."); idx > 0 {
			name = name[:idx]
		}
	}

	name = strings.ReplaceAll(name, "_", " ")

	name = stripNoise(name)

	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, ".", " ")

	name = stripNoise(name)

	name = trailGroupPost.ReplaceAllString(name, "")

	fields := strings.Fields(name)
	name = strings.Join(fields, " ")

	if len(name) > 80 {
		name = name[:80]
	}

	if stripExt {
		name = FormatVideoName(name)
	}

	return name
}

func FormatVideoName(name string) string {
	re := regexp.MustCompile(`(?i)^(.*?)\s*(S\d{2}E\d{2})\s*(.*)$`)
	m := re.FindStringSubmatch(name)
	if len(m) == 4 {
		ep := strings.ToUpper(m[2])
		title := strings.TrimSpace(m[3])
		if title != "" {
			return fmt.Sprintf("%s - %s", ep, title)
		}
		return ep
	}
	return name
}

func EpisodeSortKey(name string) int {
	re := regexp.MustCompile(`S(\d{2})E(\d{2})`)
	m := re.FindStringSubmatch(name)
	if len(m) == 3 {
		s, _ := strconv.Atoi(m[1])
		e, _ := strconv.Atoi(m[2])
		return s*10000 + e
	}
	return -1
}

func IsGarbageName(name string) bool {
	if len(name) < 3 {
		return true
	}

	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "sample") && len(name) < 20 {
		return true
	}

	letterRatio := 0
	for _, c := range name {
		if unicode.IsLetter(c) {
			letterRatio++
		}
	}
	if len(name) > 0 && float64(letterRatio)/float64(len(name)) < 0.3 {
		return true
	}

	return false
}

func ExtractEpisode(name string) string {
	re := regexp.MustCompile(`(?i)(S\d{2}(E\d{2})*)`)
	m := re.FindStringSubmatch(name)
	if len(m) > 1 {
		return strings.ToUpper(m[1])
	}
	return ""
}

func SmartRename(videos []types.SyncVideo, prefix string) {
	episodeCount := 1
	prefixText := prefix
	if prefix == "" || prefix == "Geral" {
		prefixText = ""
	}

	for i, v := range videos {
		if IsGarbageName(v.Name) {
			ep := ExtractEpisode(v.Id)
			if ep != "" {
				if prefixText != "" {
					videos[i].Name = fmt.Sprintf("%s %s", prefixText, ep)
				} else {
					videos[i].Name = ep
				}
			} else {
				if prefixText != "" {
					videos[i].Name = fmt.Sprintf("%s - Episódio %02d", prefixText, episodeCount)
				} else {
					videos[i].Name = fmt.Sprintf("Episódio %02d", episodeCount)
				}
			}
			episodeCount++
		}
	}
}
