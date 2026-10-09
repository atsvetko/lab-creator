"use strict";
(function () {
const TOKEN = new URLSearchParams(location.search).get("t") || "";
const $ = id => document.getElementById(id);
const esc = s => String(s==null?"":s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
let state = null, es = null, lab = null;

async function api(path, body){
  const o = {headers:{"X-Lab-Token":TOKEN},cache:"no-store"};
  if(body!==undefined){o.method="POST";o.headers["Content-Type"]="application/json";o.body=JSON.stringify(body);}
  const r = await fetch(path+(path.includes("?")?"&":"?")+"t="+TOKEN, o);
  return r.json().catch(()=>({ok:false,error:"HTTP "+r.status}));
}

async function refresh(){
  const s = await api("/api/state");
  if(!s.ok) return;
  state = s; lab = s.lab;
  $("labname").textContent = lab.name || "";
  $("hostbanner").classList.toggle("hidden", s.host_ok);
  if(!s.host_ok) $("hostbanner").textContent = "⚠ "+s.host_msg+" — build, config and the UI work here; VM actions need the Windows host.";
  $("credsok").classList.toggle("hidden", !s.creds_set);
  $("builtline").textContent = s.built ? ("built "+s.built.version+" ("+s.built.commit+")") : "not built yet";
  renderRows();
  const busy = s.busy;
  document.querySelectorAll("button.btn, .rowact button").forEach(b=>{ if(b.id!=="quit"&&!b.classList.contains("keepon")) b.disabled = busy; });
  if(s.job){ $("jobline").textContent = s.job.title+" · "+s.job.status; tail(s.job.id); }
}

function renderRows(){
  const tb = $("rows"); tb.innerHTML = "";
  (state.machines||[]).forEach(m=>{
    const dot = m.vm_state==="Running"?"run":(m.vm_state?"off":"none");
    const vm = m.vm_state || (state.host_ok?"absent":"—");
    const res = m.result ? `<span class="${m.result.pass?'pass':'fail'}">${m.result.pass?'PASS':'FAIL'}</span> <span class="hint">${esc(m.result.summary)}</span>` : "<span class=hint>—</span>";
    const base = m.has_baseline ? "✓" : "<span class=hint>—</span>";
    const acts = ["create","provision","snapshot","reset","start","stop","scan"].map(a=>`<button data-m="${esc(m.name)}" data-a="${a}">${a}</button>`).join("");
    const tr = document.createElement("tr");
    tr.innerHTML = `<td><b>${esc(m.name)}</b></td><td>${esc(m.kind)}</td><td>${esc(m.domain)}</td><td class=mono>${esc(m.ip)}</td>`+
      `<td><span class="dot ${dot}"></span>${esc(vm)}</td><td>${base}</td><td>${res}</td><td><div class="rowact">${acts}</div></td>`;
    tb.appendChild(tr);
  });
  tb.querySelectorAll(".rowact button").forEach(b=>b.onclick=()=>machine(b.dataset.m,b.dataset.a));
}

function line(ev){
  const c = $("console");
  const t = ev;
  let cls = ""; if(t.startsWith("$ "))cls="cmd"; else if(t.startsWith("ERROR"))cls="err"; else if(t.startsWith("» "))cls="me";
  const span = document.createElement("span");
  span.className = cls; span.textContent = t+"\n"; c.appendChild(span);
  c.scrollTop = c.scrollHeight;
}
function tail(id){
  if(es){es.close();es=null;}
  $("console").innerHTML = "";
  es = new EventSource("/api/logs?job="+encodeURIComponent(id)+"&t="+TOKEN);
  es.addEventListener("log", e=>line(JSON.parse(e.data)));
  es.addEventListener("done", e=>{ line("— "+JSON.parse(e.data)+" —"); es.close(); es=null; setTimeout(refresh,500); });
  es.onerror = ()=>{ if(es){es.close();es=null;} };
}

async function start(call){
  const r = await call();
  if(!r.ok){ line("ERROR: "+r.error); return; }
  if(r.job) tail(r.job);
  setTimeout(refresh, 300);
}
const machine = (name,action)=>start(()=>api("/api/machine",{name,action}));

$("savecreds").onclick = async ()=>{
  await api("/api/creds",{AdminPassword:$("c_admin").value,AuditPassword:$("c_audit").value,AuditUser:$("c_user").value});
  $("c_admin").value=""; $("c_audit").value=""; refresh();
};
$("build").onclick = ()=>start(()=>api("/api/build",{ref:$("ref").value.trim()}));
$("runall").onclick = ()=>start(()=>api("/api/run-all",{}));

/* ---- config editor ---- */
$("editcfg").onclick = ()=>{ const c=$("cfgcard"); c.classList.toggle("hidden"); if(!c.classList.contains("hidden")) fillCfg(); };
function fillCfg(){
  $("cfgpath").textContent = ""; $("cfgerr").classList.add("hidden");
  $("f_repo").value=lab.repo_dir||""; $("f_switch").value=lab.switch||""; $("f_work").value=lab.work_dir||""; $("f_ref").value=lab.ref||"";
  const mr = $("mrows"); mr.innerHTML="";
  (lab.machines||[]).forEach((m,i)=>mr.appendChild(mrow(m,i)));
}
function mrow(m,i){
  const d = document.createElement("div"); d.className="mrow"; d.dataset.i=i;
  const f=(k,v,ph)=>`<label>${k}<input class="inp m_${k}" value="${esc(v==null?'':v)}" placeholder="${ph||''}"></label>`;
  d.innerHTML =
    f("name",m.name)+
    `<label>kind<select class="inp m_kind"><option${m.kind==='windows-dc'?' selected':''}>windows-dc</option><option${m.kind==='alt-dc'?' selected':''}>alt-dc</option></select></label>`+
    f("domain",m.domain,"da.test")+f("ip",m.ip,"10.55.0.10")+
    `<button class="del">✕</button>`+
    f("iso",m.iso,"path to install ISO")+f("forest_mode",m.forest_mode,"Win2016")+
    f("expect",m.expect,"testdata/lab/windows-expect.yaml")+f("cpu",m.cpu)+f("mem_gb",m.mem_gb)+f("disk_gb",m.disk_gb);
  d.querySelector(".del").onclick=()=>{ lab.machines.splice(i,1); fillCfg(); };
  return d;
}
$("addm").onclick = ()=>{ lab.machines=lab.machines||[]; lab.machines.push({name:"NEW",kind:"windows-dc",domain:"da.test",ip:"10.55.0.20",prefix:24,cpu:2,mem_gb:4,disk_gb:60,forest_mode:"Win2016"}); fillCfg(); };
$("savecfg").onclick = async ()=>{
  const g=id=>$(id).value.trim();
  const next = {name:lab.name,subnet:lab.subnet,repo_dir:g("f_repo"),switch:g("f_switch"),work_dir:g("f_work"),ref:g("f_ref"),machines:[]};
  document.querySelectorAll("#mrows .mrow").forEach(d=>{
    const v=k=>{const e=d.querySelector(".m_"+k);return e?e.value.trim():"";};
    const n=k=>parseInt(v(k)||"0",10);
    next.machines.push({name:v("name"),kind:v("kind"),domain:v("domain"),ip:v("ip"),iso:v("iso"),
      forest_mode:v("forest_mode"),expect:v("expect"),prefix:24,cpu:n("cpu"),mem_gb:n("mem_gb"),disk_gb:n("disk_gb")});
  });
  const r = await api("/api/config", next);
  if(!r.ok){ const e=$("cfgerr"); e.textContent=r.error; e.classList.remove("hidden"); return; }
  $("cfgcard").classList.add("hidden"); refresh();
};

/* ---- chrome ---- */
const root=document.documentElement;
$("theme").onclick=()=>{ const n=root.getAttribute("data-theme")==="dark"?"light":"dark"; root.setAttribute("data-theme",n); try{localStorage.setItem("lab-theme",n)}catch(e){} };
$("quit").onclick=async()=>{ await api("/api/quit",{}); document.body.innerHTML='<p style="padding:40px;font:16px system-ui">Lab control panel stopped. You can close this tab.</p>'; };
try{const t=localStorage.getItem("lab-theme"); if(t)root.setAttribute("data-theme",t);}catch(e){}
refresh(); setInterval(()=>{ if(!es) refresh(); }, 5000);
})();
