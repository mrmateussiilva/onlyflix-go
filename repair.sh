#!/bin/bash
DIR="$(dirname "$0")"

LOCAL_PATH="$(grep '^LOCAL_PATH=' "$DIR/.env" | cut -d= -f2- | tr -d '\r')"
LOCAL_PATH="$(echo "$LOCAL_PATH" | xargs)"

if [ -z "$LOCAL_PATH" ]; then
  echo "LOCAL_PATH nao encontrado no .env"
  exit 1
fi

if [ ! -d "$LOCAL_PATH" ]; then
  echo "ERRO: '$LOCAL_PATH' nao e um diretorio valido"
  exit 1
fi

echo "VARRENDO: $LOCAL_PATH"
echo

find "$LOCAL_PATH" -type f \( -iname "*.mp4" -o -iname "*.mov" -o -iname "*.m4v" -o -iname "*.avi" -o -iname "*.mkv" \) -print0 | while IFS= read -r -d '' f; do
  label=$(echo "$f" | sed 's|.*/||')
  printf "\n--- %s\n" "$label"
  printf "PATH: %s\n" "$f"

  err=$(ffmpeg -v error -i "$f" -f null - 2>&1)
  rc=$?

  first_err=$(echo "$err" | head -1)

  if [ $rc -eq 0 ]; then
    echo "OK"
  elif echo "$first_err" | grep -qi "moov atom not found"; then
    echo "REPARANDO (remux)..."
    tmp="${f}.repair.tmp"
    if ffmpeg -y -analyzeduration 500M -probesize 500M -i "$f" -c copy -movflags +faststart "$tmp" 2>/dev/null; then
      mv "$tmp" "$f"
      echo "REPARADO"
    else
      rc2=$?
      echo "FALHA: ffmpeg exit $rc2"
      rm -f "$tmp"
    fi
  elif echo "$first_err" | grep -qi "No such file or directory"; then
    echo "PULANDO: arquivo inexistente (nome invalido?)"
  else
    echo "ERRO: $first_err"
  fi
done

echo
echo "FIM"
