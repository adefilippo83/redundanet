// The how-to film is one function of time: draw(T) paints second T of the
// recording. The page plays it live; the renderer calls window.seek(T) for
// each frame. The screens of the agent's page and of the join form are
// real screenshots (shots/), taken from the page's preview with sample data.
(function () {
  "use strict";
  const S = window.STRINGS, L = S.labels;
  const DURATION = S.duration;
  const cv = document.getElementById("c");
  const g = cv.getContext("2d");
  const C = {
    bg: "#0f172a", card: "#1e293b", sunk: "#0b1120", line: "#334155", fg: "#f8fafc", muted: "#94a3b8",
    pri: "#6366f1", priD: "#4f46e5", priL: "#a5b4fc", ok: "#22c55e", okD: "#15803d", bad: "#ef4444", amber: "#f59e0b",
  };
  const FONT = "Inter, -apple-system, BlinkMacSystemFont, sans-serif";
  const MONO = "'JetBrains Mono', Menlo, monospace";

  const clamp = (x) => Math.max(0, Math.min(1, x));
  const seg = (t, a, b) => clamp((t - a) / (b - a));
  const ease = (x) => (x < 0.5 ? 4 * x * x * x : 1 - Math.pow(-2 * x + 2, 3) / 2);
  const e = (t, a, b) => ease(seg(t, a, b));
  const lerp = (a, b, x) => a + (b - a) * x;
  const pop = (x) => (x <= 0 ? 0 : x >= 1 ? 1 : 1 + 2.2 * Math.pow(x - 1, 3) + 1.2 * Math.pow(x - 1, 2));
  const win = (t, a, b, f) => Math.min(seg(t, a, a + (f || 0.4)), 1 - seg(t, b - (f || 0.4), b));

  function rr(x, y, w, h, r) { g.beginPath(); g.roundRect(x, y, w, h, r); }
  function fill(c) { g.fillStyle = c; g.fill(); }
  function stroke(c, w) { g.strokeStyle = c; g.lineWidth = w; g.stroke(); }
  function at(alpha, fn) {
    if (alpha <= 0.003) return;
    g.save(); g.globalAlpha *= clamp(alpha); fn(); g.restore();
  }
  function text(s, x, y, size, weight, color, align, font) {
    g.font = `${weight} ${size}px ${font || FONT}`;
    g.textAlign = align || "center"; g.textBaseline = "middle"; g.fillStyle = color;
    g.fillText(s, x, y);
  }
  function width(s, size, weight, font) { g.font = `${weight} ${size}px ${font || FONT}`; return g.measureText(s).width; }

  // The real screens.
  const SHOTS = {};
  const BOX = window.BOXES || {};
  const names = ["welcome", "phrase", "typed", "written", "empty", "empty-in", "protected-in", "form", "form-filled", "form-send"];
  window.ready = Promise.all(names.map((n) => new Promise((res) => {
    const im = new Image(); im.onload = res; im.onerror = res; im.src = `shots/${n}.png`; SHOTS[n] = im;
  }))).then(() => (document.fonts ? document.fonts.ready : null));

  // ---- pieces of the picture -------------------------------------------

  function pill(s, x, y, color, alpha, align) {
    at(alpha, () => {
      const w = width(s, 30, 600) + 56;
      const x0 = align === "right" ? x - w : align === "center" ? x - w / 2 : x;
      rr(x0, y - 27, w, 54, 27); fill(color);
      text(s, x0 + w / 2, y + 1, 30, 600, C.fg);
    });
  }

  function topBar(step, where, alpha) {
    at(alpha, () => {
      if (step) {
        text(`${L.step} ${step} ${L.of} 5`, 70, 58, 34, 700, C.fg, "left");
        for (let i = 1; i <= 5; i++) {
          rr(300 + (i - 1) * 62, 50, 48, 14, 7); fill(i < step ? C.ok : i === step ? C.pri : C.line);
        }
      }
      if (where === "laptop") pill(L.onLaptop, 1850, 58, C.priD, 1, "right");
      if (where === "server") pill(L.onServer, 1850, 58, C.okD, 1, "right");
    });
  }

  function laptopIcon(x, y, s, alpha) {
    at(alpha, () => {
      g.save(); g.translate(x, y); g.scale(s, s);
      rr(-95, -70, 190, 124, 12); fill(C.card); stroke(C.priL, 5);
      g.beginPath(); g.moveTo(-122, 64); g.lineTo(122, 64); g.lineTo(108, 84); g.lineTo(-108, 84); g.closePath(); fill(C.card); stroke(C.priL, 5);
      g.restore();
    });
  }
  function paperIcon(x, y, s, alpha, lines) {
    at(alpha, () => {
      g.save(); g.translate(x, y); g.scale(s, s);
      rr(-70, -92, 140, 184, 8); fill("#f1f5f9");
      g.lineCap = "round";
      const n = lines === undefined ? 5 : lines;
      for (let i = 0; i < 6; i++) {
        const k = clamp(n - i);
        if (k <= 0) continue;
        g.beginPath(); g.moveTo(-46, -58 + i * 24); g.lineTo(-46 + (i % 2 ? 74 : 92) * k, -58 + i * 24); stroke("#475569", 6);
      }
      g.restore();
    });
  }
  function accountIcon(x, y, s, alpha) {
    at(alpha, () => {
      g.save(); g.translate(x, y); g.scale(s, s);
      g.beginPath(); g.arc(0, 0, 88, 0, 7); fill(C.card); stroke(C.priL, 5);
      g.beginPath(); g.arc(0, -22, 30, 0, 7); fill(C.priL);
      g.beginPath(); g.arc(0, 78, 56, Math.PI * 1.1, Math.PI * 1.9); g.closePath(); fill(C.priL);
      g.restore();
    });
  }
  function serverIcon(x, y, s, alpha) {
    at(alpha, () => {
      g.save(); g.translate(x, y); g.scale(s, s);
      for (let i = 0; i < 2; i++) {
        const yy = -74 + i * 78;
        rr(-110, yy, 220, 66, 12); fill(C.card); stroke("#86efac", 5);
        g.beginPath(); g.arc(78, yy + 33, 8, 0, 7); fill(C.ok);
        g.lineCap = "round"; g.beginPath(); g.moveTo(-80, yy + 33); g.lineTo(-10, yy + 33); stroke(C.line, 8);
      }
      g.restore();
    });
  }
  function check(x, y, s, alpha) {
    at(alpha, () => {
      g.beginPath(); g.arc(x, y, 30 * s, 0, 7); fill(C.ok);
      g.lineCap = "round"; g.lineJoin = "round";
      g.beginPath(); g.moveTo(x - 13 * s, y + 1 * s); g.lineTo(x - 3 * s, y + 11 * s); g.lineTo(x + 14 * s, y - 9 * s);
      stroke(C.fg, 7 * s);
    });
  }

  // A terminal: rows are { s, color, typed (0 to 1), mark: [from, to] }.
  function terminal(x, y, w, h, title, rows, alpha, size, accent) {
    at(alpha, () => {
      rr(x, y, w, h, 18); fill(C.sunk); stroke(accent || C.line, 3);
      g.save(); rr(x, y, w, 52, [18, 18, 0, 0]); g.clip(); g.fillStyle = C.card; g.fillRect(x, y, w, 52); g.restore();
      ["#ef4444", "#f59e0b", "#22c55e"].forEach((c, i) => { g.beginPath(); g.arc(x + 32 + i * 28, y + 26, 8, 0, 7); fill(c); });
      text(title, x + w / 2, y + 27, 24, 500, C.muted);
      const fs = size || 32, lh = fs * 1.5;
      g.save(); rr(x, y + 52, w, h - 52, 0); g.clip();
      const cw = width("M", fs, 400, MONO);
      let yy = y + 52 + 24 + lh / 2;
      for (const r of rows) {
        if (r.show !== undefined && r.show <= 0) continue;
        const n = r.typed === undefined ? r.s.length : Math.round(r.s.length * clamp(r.typed));
        at(r.show === undefined ? 1 : r.show, () => {
          if (r.mark && n >= r.mark[1] && r.markA > 0) at(r.markA, () => {
            rr(x + 28 + r.mark[0] * cw - 6, yy - lh / 2 + 4, (r.mark[1] - r.mark[0]) * cw + 12, lh - 8, 8); fill("rgba(245,158,11,0.28)"); stroke(C.amber, 3);
          });
          text(r.s.slice(0, n), x + 28, yy, fs, r.bold ? 700 : 400, r.color || C.fg, "left", MONO);
          if (r.cursor) { g.fillStyle = C.fg; g.fillRect(x + 28 + n * cw + 2, yy - fs * 0.55, cw * 0.6, fs * 1.1); }
        });
        yy += lh * (r.gap || 1);
      }
      g.restore();
    });
  }

  // A browser window showing one of the real screens. The screens are
  // 1200 wide; cropY picks the 620 rows shown.
  const FX = 240, FY = 118, FW = 1440, FH = 744, SC = 1.2, CH = 44;
  function browser(shot, address, alpha, cropY, dy) {
    at(alpha, () => {
      const y = FY + (dy || 0);
      rr(FX - 3, y - 3, FW + 6, FH + CH + 6, 16); fill(C.line);
      g.save(); rr(FX, y, FW, FH + CH, 14); g.clip();
      g.fillStyle = C.card; g.fillRect(FX, y, FW, CH);
      ["#ef4444", "#f59e0b", "#22c55e"].forEach((c, i) => { g.beginPath(); g.arc(FX + 28 + i * 24, y + CH / 2, 7, 0, 7); fill(c); });
      rr(FX + 330, y + 8, FW - 660, CH - 16, 14); fill(C.sunk);
      text(address, FX + FW / 2, y + CH / 2 + 1, 20, 500, C.muted);
      const im = SHOTS[shot];
      if (im && im.naturalWidth) g.drawImage(im, 0, (cropY || 0) * 2, 2400, 620 * 2, FX, y + CH, FW, FH);
      g.restore();
    });
  }
  // The place on the canvas of a box of a screen.
  function place(shot, key, cropY) {
    const b = (BOX[shot] || {})[key];
    if (!b) return { x: FX, y: FY + CH, w: 10, h: 10 };
    return { x: FX + b.x * SC, y: FY + CH + (b.y - (cropY || 0)) * SC, w: b.width * SC, h: b.height * SC };
  }
  function ring(p, alpha, t, pad) {
    at(alpha, () => {
      const k = pad === undefined ? 10 : pad, pulse = 3 * Math.sin(t * 5);
      rr(p.x - k - pulse, p.y - k - pulse, p.w + 2 * (k + pulse), p.h + 2 * (k + pulse), 14); stroke(C.amber, 6);
    });
  }
  function tag(s, x, y, alpha, align, color) {
    at(alpha, () => {
      const w = width(s, 28, 600) + 36;
      const x0 = align === "right" ? x - w : align === "center" ? x - w / 2 : x;
      rr(x0, y - 24, w, 48, 12); fill(color || C.amber);
      text(s, x0 + w / 2, y + 1, 28, 600, color ? C.fg : "#111827");
    });
  }
  // A pointer that travels to a spot and clicks at time c.
  function pointer(from, to, t, a, c) {
    const k = e(t, a, c - 0.25);
    const x = lerp(from.x, to.x, k), y = lerp(from.y, to.y, k);
    at(win(t, a - 0.2, c + 0.7, 0.2), () => {
      at(win(t, c, c + 0.6, 0.15), () => { g.beginPath(); g.arc(x, y, 14 + 40 * seg(t, c, c + 0.6), 0, 7); stroke(C.amber, 5); });
      g.save(); g.translate(x, y);
      g.beginPath(); g.moveTo(0, 0); g.lineTo(0, 34); g.lineTo(9, 26); g.lineTo(16, 42); g.lineTo(22, 39); g.lineTo(15, 24); g.lineTo(27, 24); g.closePath();
      fill(C.fg); g.lineJoin = "round"; stroke("#0f172a", 3);
      g.restore();
    });
  }
  const mid = (p) => ({ x: p.x + p.w / 2, y: p.y + p.h / 2 });

  function captions(t) {
    for (const c of S.captions) {
      const a = win(t, c.from, c.to, 0.3);
      if (a <= 0) continue;
      at(a, () => {
        const n = c.lines.length;
        c.lines.forEach((ln, i) => text(ln, 960, 994 + (i - (n - 1) / 2) * 56, 42, 600, C.fg));
      });
    }
  }

  // ---- the scenes, on the film's own clock --------------------------------

  const CMD_MAC = "curl -fsSL https://redundanet.com/install.sh | sh";
  const CMD_WIN = "irm https://redundanet.com/install.ps1 | iex";
  const CMD_NODE = "curl -fsSL https://redundanet.com/install.sh | sudo sh -s -- --node /mnt/storage";
  const CMD_PAIR = "redundanet device pair --storage";
  const CODE = "rn3:pair:mfzwizlomvsxg5dboj2g63tfon2gk4r7";

  function intro(t) {
    const a = win(t, 0, 14.2, 0.4);
    at(a, () => {
      const up = e(t, 3.6, 4.4);
      at(1 - up * 0.0, () => {
        text(L.title, 960, lerp(430, 150, up), lerp(104, 60, up), 700, C.fg);
        at(1 - up, () => text(L.subtitle, 960, 540, 46, 500, C.muted));
      });
      const items = [[laptopIcon, L.laptop, 4.6], [paperIcon, L.paper, 5.9], [accountIcon, L.account, 7.1], [serverIcon, L.server, 9.6]];
      items.forEach(([icon, label, t0], i) => {
        const x = 300 + i * 440, k = pop(seg(t, t0, t0 + 0.5));
        at(seg(t, t0, t0 + 0.3), () => {
          icon(x, 470, 0.95 * lerp(0.8, 1, k), 1);
          const parts = label.split("|");
          parts.forEach((s, j) => text(s, x, 640 + j * 44, 32, 600, C.fg));
          if (i === 3) tag(L.optional, x, 640 + parts.length * 44 + 20, seg(t, t0 + 0.6, t0 + 1.0), "center", C.okD);
        });
      });
    });
  }

  function step1(t) {
    const a = win(t, 14, 32.2, 0.4);
    if (a <= 0) return;
    at(a, () => {
      topBar(1, "laptop", 1);
      const away = e(t, 25.6, 26.6);
      at(1 - away, () => {
        // How to open a terminal, then the two lines.
        L.openTerminal.forEach((s, i) => at(win(t, 14.4 + i * 0.25, 20.2, 0.3), () => text(s, 960, 640 + i * 56, 34, 500, C.muted)));
        const out = [
          { s: "downloading redundanet 3.0.38 for darwin/arm64", color: C.muted, show: seg(t, 19.6, 19.8) },
          { s: "installed redundanet 3.0.38", color: C.muted, show: seg(t, 20.6, 20.8) },
          { s: "the agent runs as a service that starts at login", color: C.muted, show: seg(t, 21.4, 21.6) },
          { s: "the page is open in your browser and walks you through the rest", color: C.ok, show: seg(t, 22.6, 22.8) },
        ];
        terminal(140, 150, 1640, 420, L.macLinux, [{ s: "$ " + CMD_MAC, typed: seg(t, 16.0, 18.4), cursor: t < 19.2, bold: true, gap: 1.2 }].concat(out), 1, 32, C.priL);
        at(e(t, 20.2, 20.8), () => {
          terminal(140, 610, 1640, 150, L.windows, [{ s: "PS> " + CMD_WIN, bold: true }], 1, 32);
        });
        at(win(t, 22.0, 25.8, 0.4), () => text(L.copy, 960, 820, 32, 500, C.priL));
      });
      browser("welcome", "127.0.0.1:4470", away, 0, lerp(160, 0, away));
    });
  }

  function step2(t) {
    const a = win(t, 32, 58.2, 0.4);
    if (a <= 0) return;
    at(a, () => {
      topBar(2, "laptop", 1);
      const toPhrase = e(t, 37.9, 38.4), toTyped = e(t, 53.6, 54.1), toDone = e(t, 57.0, 57.4);
      browser("welcome", "127.0.0.1:4470", 1 - toPhrase);
      browser("phrase", "127.0.0.1:4470", toPhrase * (1 - toTyped));
      browser("typed", "127.0.0.1:4470", toTyped * (1 - toDone));
      browser("written", "127.0.0.1:4470", toDone);
      if (t < 38.2) {
        ring(place("welcome", "card"), win(t, 33.0, 35.2, 0.3), t, 6);
        const b = place("welcome", "button");
        ring(b, win(t, 35.2, 37.9, 0.3), t);
        pointer({ x: 1300, y: 760 }, mid(b), t, 35.4, 37.3);
      } else if (t < 53.9) {
        ring(place("phrase", "words"), win(t, 38.6, 53.6, 0.4), t);
        // The sheet of paper the words go on.
        const pa = win(t, 43.0, 53.6, 0.5);
        at(pa, () => {
          rr(1470, 250, 400, 470, 22); fill(C.card); stroke(C.line, 3);
          paperIcon(1670, 440, 1.25, 1, seg(t, 43.6, 47.6) * 6);
          text(L.writeDown, 1670, 630, 32, 600, C.fg);
          at(e(t, 48.2, 48.8), () => { rr(1490, 664, 360, 6, 3); fill(C.bad); });
        });
        tag(L.noReset, 960, 868, win(t, 48.2, 53.4, 0.4), "center", "#b91c1c");
      } else if (t < 57.2) {
        ring(place("typed", "fields"), win(t, 54.2, 56.2, 0.3), t);
        const b = place("typed", "button");
        pointer({ x: 1200, y: 800 }, mid(b), t, 55.2, 56.7);
      } else {
        check(960, 640, 2.2, pop(seg(t, 57.3, 57.8)));
      }
    });
  }

  function step3(t) {
    const a = win(t, 58, 92.2, 0.4);
    if (a <= 0) return;
    at(a, () => {
      const split = e(t, 75.0, 76.2);
      topBar(3, split < 0.5 ? "server" : null, 1);
      // The server's terminal: whole, then the left half.
      const x = lerp(140, 50, split), w = lerp(1640, 880, split), y = lerp(170, 220, split), h = lerp(560, 560, split);
      const fs = lerp(30, 23, split);
      const rows = [
        { s: "# " + CMD_NODE, typed: seg(t, 63.4, 67.4), cursor: t < 68.2, bold: true, mark: [2 + CMD_NODE.length - 12, 2 + CMD_NODE.length], markA: win(t, 68.0, 75.0, 0.3), gap: 1.3 },
        { s: "installed redundanet 3.0.38", color: C.muted, show: seg(t, 69.6, 69.8) },
        { s: "On a device of yours, with the phrase at hand:", color: C.muted, show: seg(t, 70.4, 70.6) },
        { s: "  redundanet device pair --storage", color: C.fg, show: seg(t, 70.4, 70.6) },
        { s: "It asks for this code, then for your phrase:", color: C.muted, show: seg(t, 71.0, 71.2), gap: 1.5 },
        { s: "  " + CODE, color: C.amber, bold: true, show: seg(t, 71.4, 71.6), gap: 1.5 },
        { s: t < 84.4 ? "Waiting..." : "paired: the node has its identity and joins", color: t < 84.4 ? C.muted : C.ok, show: seg(t, 71.8, 72.0) },
      ];
      at(seg(t, 59.8, 60.4), () => terminal(x, y, w, h, "root@my-server", rows, 1, fs, "#86efac"));
      at(win(t, 58.4, 63.2, 0.4), () => {
        serverIcon(960, 420, 1.3, 1);
        tag(L.skip, 960, 610, 1, "center", C.card);
      });
      // The folder, called out.
      at(win(t, 68.0, 74.8, 0.3) * (1 - split), () => {
        const cw = width("M", 30, 400, MONO);
        tag(L.folder, 140 + 28 + (2 + CMD_NODE.length - 6) * cw, 328, 1, "center");
      });
      at(win(t, 71.6, 74.8, 0.3) * (1 - split), () => tag(L.code, 1500, 525, 1, "center"));
      // The laptop's terminal, on the right.
      at(split, () => {
        pill(L.onServer, 490, 170, C.okD, 1, "center");
        pill(L.onLaptop, 1430, 170, C.priD, 1, "center");
        const lr = [
          { s: "$ " + CMD_PAIR, typed: seg(t, 76.6, 78.4), cursor: t < 78.8, bold: true, gap: 1.3 },
          { s: "Pairing code shown by the new device:", color: C.muted, show: seg(t, 79.0, 79.2) },
          { s: "  " + CODE.slice(0, 30) + "...", color: C.amber, typed: seg(t, 79.6, 80.8), show: seg(t, 79.5, 79.6), gap: 1.3 },
          { s: "Recovery phrase (12 words):", color: C.muted, show: seg(t, 81.2, 81.4), gap: 1.6 },
          { s: "paired device my-server (client, storage)", color: C.ok, show: seg(t, 84.2, 84.4) },
        ];
        terminal(990, 220, 880, 560, "your laptop", lr, 1, 23, C.priL);
        tag("Nothing shows while you type. That is normal.", 1430, 600, win(t, 81.6, 84.0, 0.3), "center");
        // The code goes from the server to the laptop by the member's own eyes.
        at(win(t, 78.6, 80.8, 0.3), () => {
          g.setLineDash([6, 14]); g.lineCap = "round";
          g.beginPath(); g.moveTo(620, 470); g.quadraticCurveTo(900, 560, 1040, 410); stroke(C.amber, 5); g.setLineDash([]);
        });
        // The certificate travels; the words stay.
        const go = e(t, 85.2, 87.4);
        at(win(t, 84.8, 91.8, 0.4), () => {
          const cx = lerp(1300, 560, go), cy = 700 - 60 * Math.sin(go * Math.PI);
          rr(cx - 120, cy - 32, 240, 64, 14); fill(C.okD); stroke("#86efac", 3);
          text(L.certificate, cx, cy + 1, 28, 600, C.fg);
          tag(L.stays, 1560, 700, seg(t, 84.8, 85.3), "center", C.priD);
        });
        check(490, 850, 1.3, pop(seg(t, 87.6, 88.1)));
        at(seg(t, 87.8, 88.3), () => text(L.joined, 560, 852, 34, 600, C.ok, "left"));
      });
    });
  }

  function step4(t) {
    const a = win(t, 92, 104.2, 0.4);
    if (a <= 0) return;
    at(a, () => {
      topBar(4, "laptop", 1);
      const toForm = e(t, 95.7, 96.2), toFilled = e(t, 98.8, 99.3), toSend = e(t, 101.6, 102.1);
      browser("empty", "127.0.0.1:4470", 1 - toForm);
      browser("form", "redundanet.com/join", toForm * (1 - toFilled));
      browser("form-filled", "redundanet.com/join", toFilled * (1 - toSend));
      browser("form-send", "redundanet.com/join", toSend);
      if (t < 95.9) {
        const b = place("empty", "request");
        ring(b, win(t, 93.0, 95.7, 0.3), t);
        pointer({ x: 900, y: 760 }, mid(b), t, 93.6, 95.2);
      } else if (t < 99.0) {
        const k = place("form", "key");
        ring(k, win(t, 96.6, 98.8, 0.3), t);
        tag(L.filled, k.x + k.w + 24, k.y + k.h / 2, win(t, 96.8, 98.8, 0.3), "left");
      } else if (t < 101.8) {
        const n = place("form-filled", "name"), d = place("form-filled", "gb");
        ring(n, win(t, 99.4, 101.6, 0.3), t); tag(L.yourName, n.x + n.w + 24, n.y + n.h / 2, win(t, 99.5, 101.6, 0.3), "left");
        ring(d, win(t, 100.2, 101.6, 0.3), t); tag(L.yourDisk, d.x + d.w + 24, d.y + d.h / 2, win(t, 100.3, 101.6, 0.3), "left");
      } else {
        const b = place("form-send", "submit");
        ring(b, win(t, 102.2, 104.0, 0.3), t);
        pointer({ x: 1300, y: 800 }, mid(b), t, 102.3, 103.4);
      }
    });
  }

  function step5(t) {
    const a = win(t, 104, 120.2, 0.4);
    if (a <= 0) return;
    at(a, () => {
      topBar(5, t > 108.8 ? "laptop" : null, 1);
      // A day goes by.
      at(win(t, 104.2, 109.2, 0.4), () => {
        const k = e(t, 105.0, 108.0);
        g.beginPath(); g.arc(960, 430, 150, 0, 7); fill(C.card); stroke(C.line, 6);
        g.beginPath(); g.arc(960, 430, 150, -Math.PI / 2, -Math.PI / 2 + k * Math.PI * 2); stroke(C.pri, 14);
        g.lineCap = "round";
        g.save(); g.translate(960, 430);
        g.save(); g.rotate(k * Math.PI * 2); g.beginPath(); g.moveTo(0, 0); g.lineTo(0, -70); stroke(C.fg, 10); g.restore();
        g.save(); g.rotate(k * Math.PI * 24); g.beginPath(); g.moveTo(0, 0); g.lineTo(0, -110); stroke(C.priL, 6); g.restore();
        g.beginPath(); g.arc(0, 0, 10, 0, 7); fill(C.fg);
        g.restore();
        text(L.wait, 960, 660, 40, 600, C.fg);
        text(L.day, 960, 720, 40, 600, C.priL);
        check(1100, 310, 1.4, pop(seg(t, 108.0, 108.5)));
      });
      const toIn = e(t, 108.9, 109.5), toSafe = e(t, 113.8, 114.4);
      browser("empty-in", "127.0.0.1:4470", toIn * (1 - toSafe));
      browser("protected-in", "127.0.0.1:4470", toSafe);
      if (t > 109 && t < 114) {
        tag(L.youAreIn, 960, 880, win(t, 109.5, 113.6, 0.3), "center", C.okD);
        const b = place("empty-in", "folder");
        ring(b, win(t, 110.6, 113.8, 0.3), t);
        pointer({ x: 1200, y: 700 }, mid(b), t, 111.2, 112.8);
      }
      if (t >= 114.4) { const b = place("protected-in", "title"); ring({ x: b.x - 90, y: b.y - 22, w: b.w + 100, h: b.h + 84 }, win(t, 114.6, 119.8, 0.4), t, 0); }
    });
  }

  function endCard(t) {
    const end = e(t, 120.3, 121.0);
    at(end, () => {
      const s = lerp(0.7, 1, pop(seg(t, 120.3, 121.0)));
      g.save(); g.translate(960, 360); g.scale(s, s);
      const grad = g.createLinearGradient(-80, -80, 80, 80);
      grad.addColorStop(0, C.pri); grad.addColorStop(1, C.ok);
      rr(-80, -80, 160, 160, 34); fill(grad);
      text("R", 0, 6, 104, 700, C.fg);
      g.restore();
    });
    at(e(t, 120.7, 121.3), () => text(S.end.name, 960, 540, 92, 700, C.fg));
    at(e(t, 121.1, 121.7), () => text(S.end.tagline, 960, 640, 42, 500, C.muted));
    at(e(t, 121.5, 122.1), () => text(S.end.address, 960, 750, 54, 600, C.priL));
    at(e(t, 123.6, 124.0), () => { g.fillStyle = C.bg; g.fillRect(0, 0, 1920, 1080); });
  }

  function warp(T) {
    const m = S.timing;
    for (let i = 1; i < m.length; i++) {
      if (T <= m[i][0]) return lerp(m[i - 1][1], m[i][1], (T - m[i - 1][0]) / (m[i][0] - m[i - 1][0]));
    }
    return m[m.length - 1][1];
  }

  function draw(T) {
    const t = warp(T);
    g.globalAlpha = 1; g.setLineDash([]);
    g.fillStyle = C.bg; g.fillRect(0, 0, 1920, 1080);
    intro(t); step1(t); step2(t); step3(t); step4(t); step5(t);
    at(1 - e(t, 120.0, 120.4), () => captions(t));
    endCard(t);
  }

  window.DURATION = DURATION;
  window.seek = (T) => draw(Math.max(0, Math.min(DURATION, T)));

  if (!/[?&]still/.test(location.search)) {
    window.ready.then(() => {
      const q = /[?&]t=([\d.]+)/.exec(location.search);
      if (q) return draw(parseFloat(q[1]));
      let start = null;
      const tick = (now) => {
        if (start === null) start = now;
        draw(((now - start) / 1000) % DURATION);
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    });
  }
})();
