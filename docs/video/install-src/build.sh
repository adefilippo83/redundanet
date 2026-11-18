#!/bin/sh
# Makes ../how-to-install.mp4 again from the files here.
# Needs Node, Chrome and ffmpeg; run it from this folder.
set -eu
npm install --no-save playwright-core

# The narration with a short hold before each step, so the viewer has a
# moment to look before the voice moves on: seconds into the recording,
# and the silence added there. The timing in strings.en.js counts on them.
cuts="12.08:0.8 24.55:0.8 30.56:0.5 43.18:0.8 55.5:0.6 61.33:0.6 66.40:1.0 76.0:0.8 80.17:0.5"
filter=""; labels=""; from=0; i=0
for c in $cuts; do
  at=${c%%:*}; hold=${c##*:}
  filter="$filter[0:a]atrim=start=$from:end=$at,asetpts=PTS-STARTPTS,apad=pad_dur=$hold[s$i];"
  labels="$labels[s$i]"; from=$at; i=$((i + 1))
done
filter="$filter[0:a]atrim=start=$from,asetpts=PTS-STARTPTS[s$i];$labels[s$i]concat=n=$((i + 1)):v=0:a=1[a]"
ffmpeg -y -loglevel error -i voice/narration.mp3 -filter_complex "$filter" -map "[a]" held.wav

node render.mjs frames
ffmpeg -y -loglevel error -framerate 30 -i frames/f%04d.png -c:v libx264 -preset slow -crf 21 -pix_fmt yuv420p silent.mp4
ffmpeg -y -loglevel error -i silent.mp4 -i held.wav \
  -filter_complex "[1:a]aresample=48000,loudnorm=I=-16:TP=-1.5:LRA=11,apad=whole_dur=98.5[a]" \
  -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 160k -t 98.5 -movflags +faststart ../how-to-install.mp4
ffmpeg -y -loglevel error -i frames/f0060.png -q:v 3 ../install-poster.jpg
rm -r frames silent.mp4 held.wav node_modules
