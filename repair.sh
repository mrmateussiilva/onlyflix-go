#!/bin/sh
DIR="$(dirname "$0")"
LOCAL_PATH="$(grep '^LOCAL_PATH=' "$DIR/.env" | cut -d= -f2-)"

if [ -z "$LOCAL_PATH" ]; then
  echo "LOCAL_PATH nao encontrado no .env"
  exit 1
fi

echo "VARRENDO: $LOCAL_PATH"
echo

find "$LOCAL_PATH" -type f \( -iname "*.mp4" -o -iname "*.mov" -o -iname "*.m4v" -o -iname "*.avi" -o -iname "*.mkv" \) | while IFS= read -r f; do
  echo "---"
  echo "ARQUIVO: $f"

  err=$(ffmpeg -v error -i "$f" -f null - 2>&1)
  if [ $? -eq 0 ]; then
    echo "OK: sem erros"
  else
    first_err=$(echo "$err" | head -1)
    echo "ERRO: $first_err"

    case "$first_err" in
      *"moov atom not found"*)
        echo "TENTANDO REPARAR (remux)..."
        tmp="${f}.repair.tmp"
        if ffmpeg -y -analyzeduration 500M -probesize 500M -i "$f" -c copy -movflags +faststart "$tmp" 2>/dev/null; then
          mv "$tmp" "$f"
          echo "REPARADO: $f"
        else
          echo "FALHA: nao foi possivel reparar"
          rm -f "$tmp"
        fi
        ;;
      *)
        echo "OUTRO TIPO DE ERRO, pulando"
        ;;
    esac
  fi
done

echo
echo "FIM"
