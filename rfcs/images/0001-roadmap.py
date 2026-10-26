"""Draw the RFC 0001 roadmap (rfcs/images/0001-roadmap.png).

Edit the rows below, then:

    python3 rfcs/images/0001-roadmap.py /tmp/roadmap.html
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new \
        --hide-scrollbars --force-device-scale-factor=2 --window-size=1360,765 \
        --screenshot=rfcs/images/0001-roadmap.png file:///tmp/roadmap.html
"""
import datetime as dt, sys, html
out=sys.argv[1]
X0, MW = 430, 56            # x of 1 Nov 2026, px per month
START = dt.date(2026,11,1)
MONTHS = 17                 # Nov 2026 .. Mar 2028 inclusive of last tick
def mx(d):
    m=(d.year-START.year)*12+(d.month-START.month)
    import calendar
    days=calendar.monthrange(d.year,d.month)[1]
    return X0+MW*(m+(d.day-1)/days)
D=dt.date
rows=[ # (label, kind, start, end, text, style)
 ("Phase 0: spike","bar",D(2026,11,16),D(2026,11,30),"Nov 16 to Nov 30",""),
 ("G0: transport and cipher decided","gate",D(2026,11,30),None,"Nov 30",""),
 ("Phase 1: agent on Tahoe","bar",D(2026,12,1),D(2027,2,28),"Dec 1 to Feb 28",""),
 ("G1a: first macOS or Windows member","gate",D(2027,2,28),None,"Feb 28","accent"),
 ("Step 3: nodes leave Docker","bar",D(2027,3,1),D(2027,5,31),"Mar 1 to May 31, members' pace",""),
 ("G1b: tinc gone everywhere","gate",D(2027,5,31),None,"May 31",""),
 ("Phase 2: native engine","bar",D(2027,3,1),D(2027,7,31),"Mar 1 to Jul 31",""),
 ("G2: native backups, 30 days","gate",D(2027,8,31),None,"Aug 31",""),
 ("Phase 3: migration, 120 days","bar",D(2027,9,1),D(2027,12,29),"Sep 1 to Dec 29",""),
 ("G3: no Tahoe left","gate",D(2027,12,29),None,"Dec 29",""),
 ("Phase 4: polish, ongoing","bar",D(2028,1,1),D(2028,3,31),"From Jan 2028",""),
]
W,TOP,RH=1360,175,50
H=TOP+RH*len(rows)+40
months=["Nov","Dec","Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec","Jan","Feb","Mar"]
s=[]
a=s.append
a(f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" font-family="-apple-system, Helvetica, Arial, sans-serif">')
a(f'<rect width="{W}" height="{H}" fill="#ffffff"/>')
a('<text x="42" y="50" font-size="26" font-weight="600" fill="#141413">The binary ships in February 2027; Tahoe is gone by the end of 2027</text>')
a('<text x="42" y="84" font-size="19" fill="#5e5d59">Plan at about 15 hours a week from November 2026; gates are tests the live network passes</text>')
for i,mn in enumerate(months):
    x=X0+MW*i
    a(f'<text x="{x}" y="140" font-size="19" fill="#5e5d59" text-anchor="middle">{mn}</text>')
    a(f'<line x1="{x}" y1="155" x2="{x}" y2="{TOP+RH*len(rows)-10}" stroke="#e8e6dc" stroke-width="1"/>')
for yr,i in (("2026",0),("2027",2),("2028",14)):
    a(f'<text x="{X0+MW*i}" y="112" font-size="19" font-weight="700" fill="#141413" text-anchor="middle">{yr}</text>')
a(f'<line x1="{X0}" y1="155" x2="{X0+MW*(len(months)-1)}" y2="155" stroke="#c9c7bd" stroke-width="2"/>')
gx=mx(D(2027,2,28))
a(f'<line x1="{gx}" y1="{TOP+RH*3-12}" x2="{gx}" y2="{TOP+RH*len(rows)-10}" stroke="#2c7be5" stroke-width="2" stroke-dasharray="5 4"/>')
right_edge=X0+MW*(len(months)-1)
for i,(lab,kind,st,en,txt,style) in enumerate(rows):
    y=TOP+RH*i+12
    isgate=kind=="gate"; accent=style=="accent"
    col="#141413" if (not isgate or accent) else "#5e5d59"
    fw="700" if accent else "400"
    fs=19 if isgate else 22
    a(f'<text x="42" y="{y+7}" font-size="{fs}" font-weight="{fw}" fill="{col}">{html.escape(lab)}</text>')
    a(f'<line x1="{X0}" y1="{y}" x2="{right_edge}" y2="{y}" stroke="#f0eee6" stroke-width="1"/>')
    tw=len(txt)*10.2
    if isgate:
        x=mx(st); c="#2c7be5" if accent else "#c9c7bd"
        a(f'<rect x="{x-11}" y="{y-11}" width="22" height="22" transform="rotate(45 {x} {y})" fill="{c}"/>')
        if x+24+tw < W-10: a(f'<text x="{x+24}" y="{y+7}" font-size="19" font-weight="{fw}" fill="{col}">{txt}</text>')
        else: a(f'<text x="{x-24}" y="{y+7}" font-size="19" fill="{col}" text-anchor="end">{txt}</text>')
    else:
        x1,x2=mx(st),min(mx(en+dt.timedelta(days=1)),right_edge+20)
        a(f'<rect x="{x1}" y="{y-13}" width="{max(x2-x1,26)}" height="26" rx="13" fill="#f3f2ee" stroke="#c9c7bd" stroke-width="2"/>')
        if x2+16+tw < W-10: a(f'<text x="{x2+16}" y="{y+7}" font-size="19" fill="#5e5d59">{txt}</text>')
        else: a(f'<text x="{x1-14}" y="{y+7}" font-size="19" fill="#5e5d59" text-anchor="end">{txt}</text>')
a('</svg>')
svg="\n".join(s)
open(out,'w').write(f'<!doctype html><html><head><meta charset="utf-8"><style>html,body{{margin:0;background:#fff}}</style></head><body>{svg}</body></html>')
print(W,H)
