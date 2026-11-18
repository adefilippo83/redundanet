# The "How it works" film

`../how-it-works.mp4` on the home page is made from the files here. Nothing in this folder is loaded by the site.

- `film.js` draws any moment of the film on a canvas: the film is one function of time, so the video comes out the same every time it is made. Open `index.html` in a browser to watch it live, or `index.html?t=12.5` for one moment.
- `strings.en.js` holds every word on screen, the text the voice reads, and the timing that makes the picture follow the recording. A translation is a copy of this file with its own recording.
- `voice/narration.mp3` is the narration, generated with ElevenLabs from the `voice` text in `strings.en.js`.
- `render.mjs` writes the frames with the Chrome installed on the machine.

The film shows six pieces and six servers on purpose and never says how many of how many: the encoding of the live network changes, the film should not.

## Making the video again

Needs Node, Chrome and ffmpeg.

```bash
cd docs/video/src
npm install --no-save playwright-core
node render.mjs frames
ffmpeg -y -framerate 30 -i frames/f%04d.png -c:v libx264 -preset slow -crf 20 -pix_fmt yuv420p silent.mp4
ffmpeg -y -i silent.mp4 -i voice/narration.mp3 \
  -filter_complex "[1:a]aresample=48000,loudnorm=I=-16:TP=-1.5:LRA=11,apad=whole_dur=32[a]" \
  -map 0:v -map "[a]" -c:v copy -c:a aac -b:a 160k -t 32 -movflags +faststart ../how-it-works.mp4
ffmpeg -y -i frames/f0390.png -q:v 3 ../poster.jpg
rm -r frames silent.mp4 node_modules
```

With a new recording, find its pauses (`ffmpeg -i voice/narration.mp3 -af silencedetect=noise=-35dB:d=0.18 -f null -`) and set `duration`, `timing` and the captions' times in `strings.en.js` to them.
