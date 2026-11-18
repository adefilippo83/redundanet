// Every word on screen and every spoken line, with its time in seconds.
// A translation is a copy of this file.
window.STRINGS = {
  // How long this language's recording makes the film, and where the film's
  // own clock (second column) stands at each moment of it (first column):
  // the picture follows the voice.
  duration: 32,
  timing: [[0, 0], [3.1, 4], [9.6, 9], [14.8, 15], [15.9, 15.2], [20.5, 21], [26.1, 26], [29.6, 29.5], [31.6, 29.55], [32, 30]],
  narration: "voice/narration.mp3",
  captions: [
    { from: 0.1, to: 3.1, lines: ["Disks fail,", "and companies change their minds."] },
    { from: 3.2, to: 9.6, lines: ["Your files are encrypted on your own device,", "and only your twelve words can open them."] },
    { from: 9.7, to: 14.8, lines: ["Each file is split into pieces", "and spread across servers run by other members."] },
    { from: 14.9, to: 20.5, lines: ["When some of those servers disappear,", "the pieces that remain still bring everything back."] },
    { from: 20.6, to: 26.2, lines: ["You share some disk space, you get backup space in return,", "and the network repairs itself."] },
  ],
  labels: {
    you: "you",
    words: "your twelve words",
    members: "other members",
    yours: "your server",
  },
  end: {
    name: "RedundaNet",
    tagline: "Community-run encrypted backup. Open source.",
    address: "redundanet.com",
  },
  // What the voice says, in one take.
  voice: "Disks fail, and companies change their minds. With Redunda Net, your files are encrypted on your own device, and only your twelve words can open them. Each file is split into pieces and spread across servers run by other members, so when some of those servers disappear, the pieces that remain still bring everything back. You share some disk space, you get backup space in return, and the network repairs itself. Redunda Net. Community-run, and open source.",
};
