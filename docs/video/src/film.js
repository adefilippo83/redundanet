// The film is one function of time: draw(t) paints second t of 30 on the
// canvas. The page plays it live; the renderer calls window.seek(t) for
// each frame, so the video is the same every time it is made.
(function () {
  "use strict";
  const DURATION = (window.STRINGS && window.STRINGS.duration) || 30;
  const cv = document.getElementById("c");
  // Two layouts of one film: wide for the site, tall (?story) for a phone
  // story, 1080 by 1920, with the top and the bottom left clear for the
  // app's own controls.
  const TALL = /[?&]story/.test(location.search);
  const W = TALL ? 1080 : 1920, H = TALL ? 1920 : 1080, CX = W / 2;
  cv.width = W; cv.height = H;
  if (TALL) cv.style.cssText = "display:block;height:100vh;width:auto;max-width:100vw;margin:0 auto";
  const g = cv.getContext("2d");
  const S = window.STRINGS;
  const C = {
    bg: "#0f172a", card: "#1e293b", sunk: "#162033", line: "#334155", fg: "#f8fafc", muted: "#94a3b8",
    pri: "#6366f1", priD: "#4f46e5", priL: "#a5b4fc", ok: "#22c55e", bad: "#ef4444", amber: "#f59e0b", teal: "#14b8a6",
  };
  const FONT = "Inter, -apple-system, BlinkMacSystemFont, sans-serif";

  const clamp = (x) => Math.max(0, Math.min(1, x));
  const seg = (t, a, b) => clamp((t - a) / (b - a));
  const ease = (x) => (x < 0.5 ? 4 * x * x * x : 1 - Math.pow(-2 * x + 2, 3) / 2);
  const e = (t, a, b) => ease(seg(t, a, b));
  const lerp = (a, b, x) => a + (b - a) * x;
  const pop = (x) => (x <= 0 ? 0 : x >= 1 ? 1 : 1 + 2.2 * Math.pow(x - 1, 3) + 1.2 * Math.pow(x - 1, 2));
  // window is 1 between a and b, with a fade of f at each end.
  const win = (t, a, b, f) => Math.min(seg(t, a, a + (f || 0.4)), 1 - seg(t, b - (f || 0.4), b));

  // The stage.
  const LAP = TALL ? { x: 540, y: 410 } : { x: 430, y: 400 };
  const MID = TALL ? { x: 540, y: 735 } : { x: 940, y: 440 };
  // Six servers and six pieces: enough that nobody counts them. The film
  // says "pieces" and "some servers", never how many of how many.
  const SRV = [0, 1, 2, 3, 4, 5].map((i) => (TALL
    ? { x: 320 + (i % 2) * 440, y: 985 + Math.floor(i / 2) * 146 }
    : { x: 1310 + (i % 2) * 320, y: 220 + Math.floor(i / 2) * 190 }));
  const LOST = [false, true, false, true, true, false];
  const SOURCE = { 1: 0, 3: 2, 4: 5 }; // where repair copies a lost piece from
  const EY = TALL ? 330 : 0;
  const MINE = TALL ? { x: 540, y: 735 } : { x: 430, y: 716 };
  const BLOCK = 184, GAP = 8, PIECE = 56;
  const QUAD = [[-1, -1], [1, -1], [-1, 0], [1, 0], [-1, 1], [1, 1]];
  const SHADE = ["#818cf8", "#6366f1", "#7c83f5", "#4f46e5", "#8b93fa", "#5b5ff0"];
  const spot = (q, gap) => ({ x: MID.x + q[0] * (PIECE + gap) / 2, y: MID.y + q[1] * (PIECE + gap) });

  function rr(x, y, w, h, r) {
    g.beginPath();
    g.roundRect(x, y, w, h, r);
  }
  function fill(c) { g.fillStyle = c; g.fill(); }
  function stroke(c, w) { g.strokeStyle = c; g.lineWidth = w; g.stroke(); }
  function at(alpha, fn) {
    if (alpha <= 0.003) return;
    g.save();
    g.globalAlpha *= clamp(alpha);
    fn();
    g.restore();
  }
  function text(s, x, y, size, weight, color, align) {
    g.font = `${weight} ${size}px ${FONT}`;
    g.textAlign = align || "center";
    g.textBaseline = "middle";
    g.fillStyle = color;
    g.fillText(s, x, y);
  }

  function laptop(alpha, tint) {
    at(alpha, () => {
      const w = 380, h = 240, x = LAP.x - w / 2, y = LAP.y - h / 2;
      rr(x, y, w, h, 18); fill(C.card); stroke(C.line, 3);
      rr(x + 14, y + 14, w - 28, h - 28, 8); fill(C.sunk);
      if (tint > 0) at(tint, () => { rr(x + 14, y + 14, w - 28, h - 28, 8); fill("rgba(239,68,68,0.38)"); });
      g.beginPath();
      g.moveTo(x - 34, y + h + 12); g.lineTo(x + w + 34, y + h + 12);
      g.lineTo(x + w + 14, y + h + 34); g.lineTo(x - 14, y + h + 34); g.closePath();
      fill(C.card); stroke(C.line, 3);
      text(S.labels.you, LAP.x, y + h + 74, 30, 500, C.muted);
    });
  }

  function file(x, y, s, alpha) {
    at(alpha, () => {
      const w = 62 * s, h = 80 * s, c = 20 * s;
      g.beginPath();
      g.moveTo(x - w / 2, y - h / 2); g.lineTo(x + w / 2 - c, y - h / 2); g.lineTo(x + w / 2, y - h / 2 + c);
      g.lineTo(x + w / 2, y + h / 2); g.lineTo(x - w / 2, y + h / 2); g.closePath();
      fill("#e2e8f0");
      g.beginPath();
      g.moveTo(x + w / 2 - c, y - h / 2); g.lineTo(x + w / 2 - c, y - h / 2 + c); g.lineTo(x + w / 2, y - h / 2 + c); g.closePath();
      fill("#94a3b8");
      g.lineCap = "round";
      for (let i = 0; i < 3; i++) {
        g.beginPath();
        g.moveTo(x - w / 2 + 12 * s, y - 6 * s + i * 14 * s);
        g.lineTo(x + w / 2 - (i === 2 ? 26 : 12) * s, y - 6 * s + i * 14 * s);
        stroke("#64748b", 5 * s);
      }
    });
  }

  // lock: open goes from 0 (closed) to 1 (open).
  function lock(x, y, s, open, color, alpha) {
    at(alpha, () => {
      g.save();
      g.translate(x, y);
      g.lineCap = "round";
      g.save();
      g.translate(-18 * s * open, -10 * s * open);
      g.rotate(-0.5 * open);
      g.beginPath();
      g.moveTo(-18 * s, -6 * s); g.lineTo(-18 * s, -22 * s);
      g.arc(0, -22 * s, 18 * s, Math.PI, 0);
      g.lineTo(18 * s, -6 * s);
      stroke(color, 9 * s);
      g.restore();
      rr(-30 * s, -8 * s, 60 * s, 46 * s, 9 * s); fill(color);
      g.beginPath(); g.arc(0, 12 * s, 6 * s, 0, 7); fill(C.priD);
      g.restore();
    });
  }

  function piece(x, y, size, color, alpha, ring) {
    at(alpha, () => {
      rr(x - size / 2, y - size / 2, size, size, size * 0.16); fill(color);
      if (ring) stroke(ring, 4);
      g.lineCap = "round";
      for (let i = 0; i < 2; i++) {
        g.beginPath();
        g.moveTo(x - size * 0.26, y - size * 0.1 + i * size * 0.22);
        g.lineTo(x + size * (i ? 0.08 : 0.26), y - size * 0.1 + i * size * 0.22);
        stroke("rgba(255,255,255,0.45)", size * 0.07);
      }
    });
  }

  // server: on goes from 0 (dark, away) to 1 (running).
  function server(p, alpha, on, glow) {
    at(alpha, () => {
      const w = 280, h = 132, x = p.x - w / 2, y = p.y - h / 2;
      at(lerp(0.32, 1, on), () => {
        if (glow > 0) at(glow, () => { rr(x - 8, y - 8, w + 16, h + 16, 24); fill("rgba(99,102,241,0.28)"); });
        rr(x, y, w, h, 18); fill(C.card); stroke(C.line, 3);
        g.lineCap = "round";
        for (let i = 0; i < 3; i++) {
          g.beginPath(); g.moveTo(x + w - 96, y + 40 + i * 26); g.lineTo(x + w - 52, y + 40 + i * 26); stroke(C.line, 6);
        }
        g.beginPath(); g.arc(x + w - 28, y + 40, 8, 0, 7); fill(on > 0.5 ? C.ok : C.bad);
      });
      if (on < 0.5) at(1 - on * 2, () => {
        g.lineCap = "round";
        g.beginPath(); g.moveTo(p.x + 62, p.y + 6); g.lineTo(p.x + 98, p.y + 42);
        g.moveTo(p.x + 98, p.y + 6); g.lineTo(p.x + 62, p.y + 42); stroke(C.bad, 8);
      });
    });
  }
  const slot = (p, k) => ({ x: p.x - 82 + (k || 0) * 78, y: p.y });

  function cloud(x, y, alpha, cross) {
    at(alpha, () => {
      g.beginPath();
      g.arc(x - 70, y + 20, 46, Math.PI * 0.5, Math.PI * 1.5);
      g.arc(x - 20, y - 26, 62, Math.PI * 1.05, Math.PI * 1.85);
      g.arc(x + 62, y + 4, 62, Math.PI * 1.4, Math.PI * 0.5);
      g.closePath();
      fill(C.card); stroke(C.muted, 5);
      at(cross, () => {
        g.lineCap = "round";
        g.beginPath(); g.moveTo(x - 34, y - 22); g.lineTo(x + 30, y + 42);
        g.moveTo(x + 30, y - 22); g.lineTo(x - 34, y + 42); stroke(C.bad, 12);
      });
    });
  }

  function warning(x, y, alpha) {
    at(alpha, () => {
      g.beginPath(); g.moveTo(x, y - 40); g.lineTo(x + 46, y + 36); g.lineTo(x - 46, y + 36); g.closePath();
      g.lineJoin = "round"; fill(C.bad); stroke(C.bad, 10);
      text("!", x, y + 8, 54, 700, C.fg);
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

  function words(alpha, t0, t) {
    at(alpha, () => {
      const w = 92, h = 30, gx = 14, gy = 14, cols = 4;
      const x0 = MID.x - (cols * w + (cols - 1) * gx) / 2, y0 = MID.y + 150;
      for (let i = 0; i < 12; i++) {
        const a = pop(seg(t, t0 + i * 0.08, t0 + i * 0.08 + 0.35));
        const x = x0 + (i % cols) * (w + gx), y = y0 + Math.floor(i / cols) * (h + gy);
        at(a, () => {
          rr(x, y, w, h, 15); fill(C.card); stroke(C.pri, 2.5);
          rr(x + 16, y + 11, w - 32 - ((i * 7) % 3) * 9, 8, 4); fill(C.priL);
        });
      }
      at(seg(t, t0 + 0.9, t0 + 1.4), () => text(S.labels.words, MID.x, y0 + 3 * (h + gy) + 22, 30, 500, C.muted));
    });
  }

  // wrap breaks a sentence into lines no wider than max.
  function wrap(s, max, size, weight) {
    g.font = `${weight} ${size}px ${FONT}`;
    const out = [];
    let line = "";
    for (const w of s.split(" ")) {
      const next = line ? line + " " + w : w;
      if (line && g.measureText(next).width > max) { out.push(line); line = w; } else line = next;
    }
    if (line) out.push(line);
    return out;
  }

  function captions(t) {
    for (const c of S.captions) {
      const a = win(t, c.from, c.to, 0.35);
      if (a <= 0) continue;
      at(a, () => {
        const lines = TALL ? wrap(c.lines.join(" "), 940, 54, 600) : c.lines;
        const n = lines.length, y = TALL ? 1478 : 978, lh = TALL ? 70 : 62;
        lines.forEach((ln, i) => text(ln, CX, y + (i - (n - 1) / 2) * lh, TALL ? 54 : 46, 600, C.fg));
      });
    }
  }

  // warp turns a moment of the recording into a moment of the film.
  function warp(T) {
    const m = S.timing;
    if (!m) return T;
    for (let i = 1; i < m.length; i++) {
      if (T <= m[i][0]) return lerp(m[i - 1][1], m[i][1], (T - m[i - 1][0]) / (m[i][0] - m[i - 1][0]));
    }
    return m[m.length - 1][1];
  }

  function draw(T) {
    const t = warp(T);
    g.globalAlpha = 1;
    g.fillStyle = C.bg;
    g.fillRect(0, 0, W, H);
    const stage = 1 - e(t, 26.0, 26.7);

    at(stage, () => {
      // 0 to 4: one disk fails, one company can change its mind.
      const failing = win(t, 1.5, 3.9, 0.3);
      const flick = failing * (0.55 + 0.45 * Math.sin(t * 19));
      laptop(e(t, 0, 0.6), flick);
      warning(LAP.x, LAP.y - 205, failing * pop(seg(t, 1.5, 1.9)));
      cloud(MID.x + (TALL ? 0 : 40), MID.y - 10, win(t, 2.1, 4.1, 0.35), e(t, 2.7, 3.0));

      // The three files: on the screen, then to the middle, then one block.
      const go = e(t, 4.2, 5.4);
      const home = e(t, 20.2, 21.0);
      const back = t >= 19.2;
      const fa = back ? e(t, 19.3, 19.9) : 1 - e(t, 5.2, 5.8);
      for (let i = 0; i < 3; i++) {
        const sx = LAP.x + (i - 1) * 96, sy = LAP.y;
        const mx = MID.x + (i - 1) * 104, my = MID.y;
        const k = back ? 1 - home : go;
        const a = back ? fa : pop(seg(t, 0.3 + i * 0.18, 0.7 + i * 0.18)) * fa * (1 - 0.5 * flick);
        file(lerp(sx, mx, k), lerp(sy, my, k), lerp(1, 1.25, k), a);
      }
      check(LAP.x + 150, LAP.y - 96, 1, pop(seg(t, 21.0, 21.4)));

      // The middle: a block, locked, then four pieces.
      const formed = e(t, 5.2, 5.8);
      const tintIn = e(t, 5.8, 6.6);
      const split = e(t, 9.6, 10.4);
      const rebuilt = e(t, 18.4, 19.2);
      const opened = e(t, 19.2, 19.9);
      const gap = t < 15 ? lerp(GAP, 34, split) : lerp(34, GAP, rebuilt);
      const away = e(t, 15.2, 16.0);
      const returned = e(t, 21.8, 22.6);
      const serverOn = LOST.map((l) => (l ? clamp(1 - away + returned) : 1));

      // Servers and their links.
      at(e(t, 9.0, 9.5), () => text(S.labels.members, TALL ? CX : 1470, SRV[0].y - 104, 30, 500, C.muted));
      SRV.forEach((p, i) => {
        const a = e(t, 9.0 + i * 0.15, 9.7 + i * 0.15);
        at(a * 0.5 * serverOn[i] * (1 - e(t, 20.4, 21.2)), () => {
          g.setLineDash([4, 14]); g.lineCap = "round";
          g.beginPath();
          if (TALL) { g.moveTo(MID.x, MID.y + 130); g.lineTo(p.x, p.y - 72); } else { g.moveTo(MID.x + 120, MID.y); g.lineTo(p.x - 150, p.y); }
          stroke(C.line, 4);
          g.setLineDash([]);
        });
      });
      SRV.forEach((p, i) => {
        const a = e(t, 9.0 + i * 0.15, 9.7 + i * 0.15);
        const land = 10.6 + i * 0.3 + 1.6;
        server(p, a, serverOn[i], win(t, land - 0.1, land + 0.7, 0.3));
      });

      // The block before it splits.
      if (t < 10.4) {
        at(formed * (1 - e(t, 9.5, 9.7)), () => {
          rr(MID.x - BLOCK / 2, MID.y - BLOCK / 2, BLOCK, BLOCK, 26);
          fill(tintIn < 1 ? mix("#e2e8f0", C.pri, tintIn) : C.pri);
        });
        lock(MID.x, MID.y + 4, 1.25, 1 - e(t, 6.0, 6.7), C.fg, formed * (1 - e(t, 9.2, 9.7)));
        words(1 - e(t, 8.6, 9.2), 6.7, t);
      }

      // The pieces: out to the servers, kept there.
      let nth = 0;
      QUAD.forEach((q, i) => {
        const from = spot(q, gap);
        const to = slot(SRV[i]);
        const k = e(t, 10.6 + i * 0.3, 12.2 + i * 0.3);
        const size = lerp(PIECE, 64, k);
        const shown = e(t, 9.5, 9.7);
        // A piece on a server that went away is gone with it; repair fills it back.
        const lost = LOST[i] ? away : 0;
        const r0 = 23.6 + nth * 0.25;
        piece(lerp(from.x, to.x, k), lerp(from.y, to.y, k), size, SHADE[i], shown * (1 - lost * 0.85) * (t < r0 || !LOST[i] ? 1 : 0));
        if (LOST[i]) {
          nth++;
          if (t >= r0) {
            const refill = e(t, r0, r0 + 1.4);
            const src = slot(SRV[SOURCE[i]]);
            piece(lerp(src.x, to.x, refill), lerp(src.y, to.y, refill), 64, SHADE[i], seg(t, r0, r0 + 0.2), refill < 1 ? C.ok : null);
            at(win(t, r0 + 1.3, r0 + 2.2, 0.3), () => { rr(to.x - 40, to.y - 40, 80, 80, 16); stroke(C.ok, 4); });
          }
        }
      });

      // 15 to 21: some servers away; the pieces left bring the file back.
      if (t >= 16.4 && t < 20.2) {
        let j = 0;
        QUAD.forEach((q, i) => {
          const h = spot(q, gap);
          const fade = 1 - opened;
          if (!LOST[i]) {
            const src = slot(SRV[i]);
            const k = e(t, 16.6 + j * 0.15, 18.2 + j * 0.15);
            j++;
            piece(lerp(src.x, h.x, k), lerp(src.y, h.y, k), lerp(64, PIECE, k), SHADE[i], seg(t, 16.4, 16.7) * fade);
          } else {
            piece(h.x, h.y, PIECE, SHADE[i], rebuilt * fade, rebuilt < 1 ? C.ok : null);
          }
        });
        lock(MID.x, MID.y + 4, 1.25, e(t, 18.9, 19.4), C.fg, win(t, 18.7, 19.8, 0.3));
      }

      // 21 to 26: your server takes other members' pieces.
      const mine = e(t, 21.2, 21.9);
      server(MINE, mine, 1, win(t, 23.2, 24.0, 0.3));
      at(mine, () => text(S.labels.yours, MINE.x, MINE.y + 100, 30, 500, C.muted));
      [[0, C.amber], [2, C.teal]].forEach(([from, color], n) => {
        const a = TALL ? { x: SRV[from].x, y: SRV[from].y - 80 } : { x: SRV[from].x - 150, y: SRV[from].y + 20 };
        const b = slot(MINE, n);
        const k = e(t, 22.0 + n * 0.35, 23.5 + n * 0.35);
        piece(lerp(a.x, b.x, k), lerp(a.y, b.y, k), 64, color, seg(t, 22.0 + n * 0.35, 22.3 + n * 0.35));
      });

      captions(T);
    });

    // 26 to 30: the name.
    const end = e(t, 26.5, 27.2);
    at(end, () => {
      const s = lerp(0.7, 1, pop(seg(t, 26.5, 27.2)));
      g.save();
      g.translate(CX, 380 + EY); g.scale(s, s);
      const grad = g.createLinearGradient(-80, -80, 80, 80);
      grad.addColorStop(0, C.pri); grad.addColorStop(1, C.ok);
      rr(-80, -80, 160, 160, 34); fill(grad);
      text("R", 0, 6, 104, 700, C.fg);
      g.restore();
    });
    at(e(t, 27.0, 27.6), () => text(S.end.name, CX, EY + 560 + 16 * (1 - e(t, 27.0, 27.6)), 92, 700, C.fg));
    at(e(t, 27.5, 28.1), () => (TALL ? S.end.tagline.split(/(?<=\.) /) : [S.end.tagline]).forEach((ln, i) => text(ln, CX, EY + 660 + i * 58, TALL ? 44 : 42, 500, C.muted)));
    at(e(t, 28.0, 28.6), () => text(S.end.address, CX, EY + (TALL ? 820 : 760), TALL ? 58 : 50, 600, C.priL));
    at(e(t, 29.6, 30.0), () => { g.fillStyle = C.bg; g.fillRect(0, 0, W, H); });
  }

  function mix(a, b, x) {
    const p = (h) => [1, 3, 5].map((i) => parseInt(h.slice(i, i + 2), 16));
    const A = p(a), B = p(b);
    return "rgb(" + A.map((v, i) => Math.round(lerp(v, B[i], x))).join(",") + ")";
  }

  window.DURATION = DURATION;
  window.SIZE = { width: W, height: H };
  window.seek = (t) => draw(Math.max(0, Math.min(DURATION, t)));

  // Played live unless the renderer asked for stills (?still).
  if (!/[?&]still/.test(location.search)) {
    const q = /[?&]t=([\d.]+)/.exec(location.search);
    if (q) {
      (document.fonts ? document.fonts.ready : Promise.resolve()).then(() => draw(parseFloat(q[1])));
    } else {
      let start = null;
      const tick = (now) => {
        if (start === null) start = now;
        draw(((now - start) / 1000) % DURATION);
        requestAnimationFrame(tick);
      };
      requestAnimationFrame(tick);
    }
  }
})();
