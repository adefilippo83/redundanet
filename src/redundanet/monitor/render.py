"""Render the network status page (plain HTML, no dependencies).

Design notes: status colors (good/warning/serious/critical) are never used
alone — every state ships an icon + word; values and labels stay in text
tokens. Light and dark modes are both explicit. The node table doubles as the
accessible/table view of the data.
"""

from __future__ import annotations

from redundanet.monitor.status import NetworkStatus

_OVERALL = {
    "ok": ("●", "All systems operational", "good"),
    "degraded": ("▲", "Degraded", "warning"),
    "down": ("✕", "Down", "critical"),
}

_CSS = """
:root {
  color-scheme: light;
  --surface: #fcfcfb; --card: #f3f2ef; --line: #e2e1dc;
  --text-1: #0b0b0b; --text-2: #52514e;
  --good: #0ca30c; --warning: #b97900; --serious: #ec835a; --critical: #d03b3b;
  --track: #e2e1dc;
}
@media (prefers-color-scheme: dark) {
  :root {
    color-scheme: dark;
    --surface: #1a1a19; --card: #242422; --line: #3a3936;
    --text-1: #ffffff; --text-2: #c3c2b7;
    --good: #0ca30c; --warning: #fab219; --serious: #ec835a; --critical: #d03b3b;
    --track: #3a3936;
  }
}
* { box-sizing: border-box; }
body { margin: 0; padding: 24px; background: var(--surface); color: var(--text-1);
       font: 15px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 880px; margin: 0 auto; }
h1 { font-size: 20px; margin: 0 0 4px; }
.sub { color: var(--text-2); font-size: 13px; margin-bottom: 20px; }
.pill { display: inline-flex; align-items: center; gap: 8px; padding: 6px 14px;
        border: 1px solid var(--line); border-radius: 999px; font-weight: 600; }
.tiles { display: flex; flex-wrap: wrap; gap: 12px; margin: 20px 0; }
.tile { flex: 1 1 150px; background: var(--card); border: 1px solid var(--line);
        border-radius: 8px; padding: 14px 16px; }
.tile .v { font-size: 26px; font-weight: 700; }
.tile .l { color: var(--text-2); font-size: 12px; text-transform: uppercase;
           letter-spacing: .04em; }
table { width: 100%; border-collapse: collapse; margin: 8px 0 20px; }
th { text-align: left; color: var(--text-2); font-size: 12px; text-transform: uppercase;
     letter-spacing: .04em; font-weight: 600; padding: 6px 10px;
     border-bottom: 1px solid var(--line); }
td { padding: 8px 10px; border-bottom: 1px solid var(--line); }
.dot { display: inline-block; width: 9px; height: 9px; border-radius: 50%;
       margin-right: 7px; vertical-align: baseline; }
.meter { background: var(--track); border-radius: 4px; height: 8px; width: 120px;
         overflow: hidden; display: inline-block; vertical-align: middle; }
.meter i { display: block; height: 100%; border-radius: 4px; }
.notes { background: var(--card); border: 1px solid var(--line); border-radius: 8px;
         padding: 12px 16px; margin: 16px 0; }
.notes li { margin: 4px 0; }
footer { color: var(--text-2); font-size: 12px; margin-top: 28px; }
footer a { color: inherit; }
code { font-size: 13px; }
.wrap { overflow-x: auto; }
"""


def _esc(value: object) -> str:
    return (
        str(value)
        .replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )


def _human_bytes(n: int) -> str:
    value = float(n)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if value < 1024 or unit == "TB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{int(value)} B"
        value /= 1024
    return f"{value:.1f} TB"


def _uptime_cell(pct: float | None) -> str:
    if pct is None:
        return '<span style="color:var(--text-2)">—</span>'
    color = "var(--good)" if pct >= 99 else "var(--warning)" if pct >= 90 else "var(--critical)"
    return (
        f'<span class="meter" role="img" aria-label="{pct}% uptime">'
        f'<i style="width:{pct}%;background:{color}"></i></span> {pct}%'
    )


def _replication_value(status: NetworkStatus) -> str:
    replication = status.replication
    if replication is None:
        return "—"
    if not replication.complete:
        # With a census missing, per-object counts would be misleadingly low.
        return f"?/{replication.objects_total}"
    return f"{replication.fully_replicated}/{replication.objects_total}"


def render_html(status: NetworkStatus) -> str:
    icon, word, tone = _OVERALL.get(status.overall, ("?", status.overall, "warning"))
    online = sum(1 for n in status.nodes if n.reachable)
    grid = status.grid
    connected = "—" if grid.storage_connected is None else str(grid.storage_connected)
    tolerance = grid.tolerable_failures
    tolerance_text = "—" if tolerance is None else str(tolerance)

    replication = status.replication
    rows = []
    for node in sorted(status.nodes, key=lambda n: n.vpn_ip):
        if node.is_self:
            state = '<span class="dot" style="background:var(--good)"></span>online (hub)'
        elif node.reachable:
            rtt = f" · {node.rtt_ms:.0f} ms" if node.rtt_ms is not None else ""
            state = f'<span class="dot" style="background:var(--good)"></span>online{rtt}'
        else:
            state = '<span class="dot" style="background:var(--critical)"></span>offline'
        stored = '<span style="color:var(--text-2)">—</span>'
        if replication and node.name in replication.per_server:
            census = replication.per_server[node.name]
            stored = f"{census.objects} obj · {_esc(_human_bytes(census.disk_used_bytes))}"
        rows.append(
            "<tr>"
            f"<td><code>{_esc(node.name)}</code></td>"
            f"<td>{state}</td>"
            f"<td>{_esc(', '.join(r.removeprefix('tahoe_').replace('tinc_vpn', 'vpn') for r in node.roles))}</td>"
            f"<td>{_esc(node.manifest_status)}</td>"
            f"<td>{stored}</td>"
            f"<td>{_uptime_cell(node.uptime_24h)}</td>"
            "</tr>"
        )

    notes_html = ""
    if status.notes:
        items = "".join(f"<li>{_esc(note)}</li>" for note in status.notes)
        notes_html = f'<div class="notes"><strong>Notes</strong><ul>{items}</ul></div>'

    upload_state = (
        "unknown"
        if grid.uploads_possible is None
        else ("yes" if grid.uploads_possible else "NO — too few servers")
    )

    return f"""<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="60">
<title>RedundaNet status</title>
<style>{_CSS}</style>
</head><body><main>
<h1>RedundaNet network status</h1>
<div class="sub">Distributed encrypted storage · updated {_esc(status.generated_at)} · refreshes every 60s</div>
<span class="pill"><span style="color:var(--{tone})">{icon}</span> {_esc(word)}</span>
<div class="tiles">
  <div class="tile"><div class="v">{online}/{len(status.nodes)}</div><div class="l">nodes online</div></div>
  <div class="tile"><div class="v">{connected}/{grid.storage_expected}</div><div class="l">storage servers</div></div>
  <div class="tile"><div class="v">{grid.shares_needed}-of-{grid.shares_total}</div><div class="l">erasure coding</div></div>
  <div class="tile"><div class="v">{tolerance_text}</div><div class="l">server failures tolerated</div></div>
  <div class="tile"><div class="v">{_replication_value(status)}</div><div class="l">objects fully replicated</div></div>
</div>
<p>New uploads require {grid.shares_happy} distinct server(s) — currently possible: <strong>{_esc(upload_state)}</strong>.</p>
{notes_html}
<h1>Nodes</h1>
<div class="wrap"><table>
<thead><tr><th>Node</th><th>VPN link</th><th>Roles</th><th>Manifest</th><th>Stored</th><th>Uptime (24h)</th></tr></thead>
<tbody>{"".join(rows)}</tbody>
</table></div>
<footer>
  <a href="/status.json">status.json</a> ·
  <a href="https://github.com/adefilippo83/redundanet">source &amp; join</a> ·
  measured from the network hub
</footer>
</main></body></html>
"""
