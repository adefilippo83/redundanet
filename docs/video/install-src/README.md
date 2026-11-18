# The "How to install" film

`../how-to-install.mp4` on the install page is made from the files here. Nothing in this folder is loaded by the site.

- `film.js` draws any moment of the film on a canvas, so the video comes out the same every time it is made. Open `index.html` in a browser to watch it live, or `index.html?t=60` for one moment.
- `strings.en.js` holds every word on screen, the text the voice reads, and the timing that makes the picture follow the recording. `narration.txt` is the same text, ready to paste. A translation is a copy of the strings file with its own recording and its own screens.
- `voice/narration.mp3` is the narration, generated with ElevenLabs. `build.sh` adds a short silence before each step.
- `shots/` are real screens: the agent's page from its preview with sample data (the recovery phrase there is "alpha bravo charlie ...", not a real one) and the join form from `docs/join.html`. `boxes.js` holds where the buttons the film points at are.
- The terminal text in `film.js` is what `install.sh`, `node install` and `device pair` print.

## Making the video again

```bash
cd docs/video/install-src
./build.sh
```

## Taking the screens again

When the agent's page or the join form changes, the screens change with it. Serve the page's preview in three states, then capture:

```bash
cd agent && go test -c -o /tmp/api.test ./internal/api && cd ../docs/video/install-src
npm install --no-save playwright-core
for s in "4481 setup" "4482 empty" "4483 protected"; do set -- $s
  REDUNDANET_PAGE_PREVIEW=127.0.0.1:$1 REDUNDANET_PAGE_STATE=$2 /tmp/api.test -test.v -test.run TestPreview -test.timeout 0 > prev-$2.log 2>&1 &
done
sleep 3
node capture-setup.mjs    # welcome, phrase, typed, written
node capture-rest.mjs     # the page once listed, the join form
kill %1 %2 %3; rm prev-*.log
```

Each login link works once, so start the previews again before a second capture. The capture renames the sample server to "my-server", sets the version shown, and writes the live network's numbers into the "Where your files are" card: change those in the two scripts when they change.

With a new recording, find its pauses (`ffmpeg -i voice/narration.mp3 -af silencedetect=noise=-35dB:d=0.3 -f null -`) and set `duration`, `timing` and the holds in `build.sh` to them.
