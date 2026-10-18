(function () {
  "use strict";

  var PAGE = 50;
  var POLL_MS = 2000;
  var TOKEN_KEY = "event-center-token";

  var state = {
    token: sessionStorage.getItem(TOKEN_KEY) || "",
    stream: "all",
    streams: {},
    globalSeq: 0,
    events: [],
    cursor: 0,
    follow: true,
    filter: "",
    selectedId: "",
    generation: 0,
    polling: false
  };

  var els = {};

  document.addEventListener("DOMContentLoaded", boot);

  function boot() {
    els.globalSeq = document.getElementById("global-seq");
    els.version = document.getElementById("version");
    els.status = document.getElementById("status");
    els.banner = document.getElementById("banner");
    els.streams = document.getElementById("streams");
    els.title = document.getElementById("stream-title");
    els.follow = document.getElementById("follow");
    els.filter = document.getElementById("filter");
    els.empty = document.getElementById("empty");
    els.events = document.getElementById("events");
    els.note = document.getElementById("window-note");
    els.detail = document.getElementById("detail-body");
    els.copy = document.getElementById("copy-detail");
    els.authForm = document.getElementById("auth-form");
    els.token = document.getElementById("token");
    els.authToggle = document.getElementById("auth-toggle");

    var params = new URLSearchParams(location.search);
    if (params.get("stream")) state.stream = params.get("stream");
    if (params.get("event")) state.selectedId = params.get("event");

    els.token.value = state.token;
    if (state.token) els.authForm.hidden = false;

    els.follow.addEventListener("change", function () {
      state.follow = els.follow.checked;
      setStatus(state.follow ? "跟随中" : "已暂停跟随", "ok");
      if (state.follow) refreshWindow();
    });
    els.filter.addEventListener("input", function () {
      state.filter = els.filter.value.trim().toLowerCase();
      renderEvents();
    });
    document.getElementById("refresh").addEventListener("click", function () {
      refreshWindow();
    });
    els.authToggle.addEventListener("click", function () {
      els.authForm.hidden = !els.authForm.hidden;
      if (!els.authForm.hidden) els.token.focus();
    });
    els.authForm.addEventListener("submit", function (ev) {
      ev.preventDefault();
      state.token = els.token.value.trim();
      if (state.token) sessionStorage.setItem(TOKEN_KEY, state.token);
      else sessionStorage.removeItem(TOKEN_KEY);
      refreshWindow();
    });
    document.getElementById("auth-clear").addEventListener("click", function () {
      state.token = "";
      els.token.value = "";
      sessionStorage.removeItem(TOKEN_KEY);
      refreshWindow();
    });
    els.copy.addEventListener("click", function () {
      var text = els.detail.textContent || "";
      if (navigator.clipboard && text) navigator.clipboard.writeText(text);
    });

    loadVersion();
    refreshWindow();
    setInterval(tick, POLL_MS);
  }

  function headers() {
    var h = { Accept: "application/json" };
    if (state.token) h.Authorization = "Bearer " + state.token;
    return h;
  }

  function api(path) {
    return fetch(path, { headers: headers() }).then(function (res) {
      if (res.status === 401) {
        var err = new Error("unauthorized");
        err.status = 401;
        throw err;
      }
      if (!res.ok) {
        return res.text().then(function (body) {
          var err = new Error(body || ("HTTP " + res.status));
          err.status = res.status;
          throw err;
        });
      }
      return res.json();
    });
  }

  function loadVersion() {
    fetch("/health").then(function (res) { return res.ok ? res.json() : null; }).then(function (body) {
      if (body && body.version) els.version.textContent = body.version;
    }).catch(function () {});
  }

  function setStatus(text, kind) {
    els.status.textContent = text;
    els.status.dataset.state = kind || "";
  }

  function setBanner(text) {
    if (!text) {
      els.banner.hidden = true;
      els.banner.textContent = "";
      return;
    }
    els.banner.hidden = false;
    els.banner.textContent = text;
  }

  function explain(err) {
    if (err && err.status === 401) {
      var wasHidden = els.authForm.hidden;
      els.authForm.hidden = false;
      if (wasHidden) els.token.focus();
      setStatus("需要凭据", "auth");
      setBanner("填写 admin token 或订阅的 API key 后才能读取事件流。");
      state.events = [];
      els.events.replaceChildren();
      document.getElementById("table-wrap").hidden = true;
      els.empty.hidden = true;
      els.note.textContent = "";
      return;
    }
    setStatus("连接失败", "error");
    setBanner(err && err.message ? err.message : "连不上 event-center。");
  }

  function headOf(stream) {
    if (stream === "all") return state.globalSeq || 0;
    return state.streams[stream] || 0;
  }

  function refreshWindow() {
    var gen = ++state.generation;
    return api("/v1/streams").then(function (body) {
      if (gen !== state.generation) return;
      state.streams = body.streams || {};
      state.globalSeq = body.global_seq || 0;
      if (state.stream !== "all" && !(state.stream in state.streams)) state.stream = "all";
      renderStreams();
      renderMeta();
      var head = headOf(state.stream);
      var after = Math.max(0, head - PAGE);
      return api("/v1/streams/" + encodeURIComponent(state.stream) + "/events?after=" + after + "&limit=" + PAGE);
    }).then(function (body) {
      if (!body || gen !== state.generation) return;
      state.events = body.events || [];
      state.cursor = body.next_cursor || 0;
      setBanner("");
      setStatus(state.follow ? "跟随中" : "已暂停跟随", "ok");
      renderEvents();
      syncDetail();
    }).catch(function (err) {
      if (gen !== state.generation) return;
      explain(err);
    });
  }

  function tick() {
    if (state.polling || document.hidden) return;
    state.polling = true;
    var gen = state.generation;
    api("/v1/streams").then(function (body) {
      if (gen !== state.generation) return;
      state.streams = body.streams || {};
      state.globalSeq = body.global_seq || 0;
      renderStreams();
      renderMeta();
      if (!state.follow) return null;
      var head = headOf(state.stream);
      if (head <= state.cursor) return null;
      return api("/v1/streams/" + encodeURIComponent(state.stream) + "/events?after=" + state.cursor + "&limit=" + PAGE);
    }).then(function (body) {
      if (!body || gen !== state.generation) return;
      var incoming = body.events || [];
      if (!incoming.length) return;
      var seen = {};
      state.events.forEach(function (ev) { seen[ev.id] = true; });
      incoming.forEach(function (ev) {
        if (!seen[ev.id]) state.events.push(ev);
      });
      state.cursor = body.next_cursor || state.cursor;
      if (state.events.length > PAGE * 4) {
        state.events = state.events.slice(state.events.length - PAGE * 4);
      }
      renderEvents();
      syncDetail();
    }).catch(function (err) {
      if (gen !== state.generation) return;
      explain(err);
    }).then(function () {
      state.polling = false;
    }, function () {
      state.polling = false;
    });
  }

  function renderMeta() {
    els.globalSeq.textContent = "全局序号 " + (state.globalSeq || 0);
    els.title.textContent = state.stream === "all" ? "全部" : state.stream;
  }

  function renderStreams() {
    var names = Object.keys(state.streams).sort();
    var items = [{ name: "all", label: "全部", seq: state.globalSeq || 0 }];
    names.forEach(function (name) {
      items.push({ name: name, label: name, seq: state.streams[name] });
    });
    els.streams.replaceChildren.apply(els.streams, items.map(function (item) {
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "stream-btn";
      if (item.name === state.stream) btn.setAttribute("aria-current", "true");
      var name = document.createElement("span");
      name.className = "stream-name";
      name.textContent = item.label;
      var seq = document.createElement("span");
      seq.className = "stream-seq";
      seq.textContent = String(item.seq);
      btn.append(name, seq);
      btn.addEventListener("click", function () { selectStream(item.name); });
      var li = document.createElement("li");
      li.append(btn);
      return li;
    }));
  }

  function selectStream(name) {
    if (state.stream === name) return;
    state.stream = name;
    state.events = [];
    state.cursor = 0;
    state.selectedId = "";
    var url = new URL(location.href);
    if (name === "all") url.searchParams.delete("stream");
    else url.searchParams.set("stream", name);
    url.searchParams.delete("event");
    history.replaceState(null, "", url);
    renderStreams();
    refreshWindow();
  }

  function visibleEvents() {
    var list = state.events.slice().sort(function (a, b) {
      return (b.seq || 0) - (a.seq || 0);
    });
    if (!state.filter) return list;
    return list.filter(function (ev) {
      var hay = ((ev.type || "") + " " + (ev.subject || "") + " " + (ev.provider || "")).toLowerCase();
      return hay.indexOf(state.filter) !== -1;
    });
  }

  function renderEvents() {
    var rows = visibleEvents();
    var head = headOf(state.stream);
    var headLabel = state.stream === "all" ? "全局序号 " : "序号到 ";
    els.note.textContent = rows.length
      ? ("当前窗口 " + rows.length + " 条 · " + headLabel + head)
      : (head ? "这条流有事件，当前窗口里没有匹配的记录。" : "");
    els.empty.hidden = rows.length > 0;
    document.getElementById("table-wrap").hidden = rows.length === 0;
    els.empty.textContent = head
      ? "当前窗口里没有匹配的事件。"
      : "还没有事件。注入进 /github-events-ingress 或 /v1/ingest/{source} 之后会出现在这里。";

    els.events.replaceChildren.apply(els.events, rows.map(function (ev) {
      var tr = document.createElement("tr");
      tr.className = "event";
      tr.tabIndex = 0;
      if (ev.id === state.selectedId) tr.setAttribute("aria-selected", "true");
      tr.append(
        cell(String(state.stream === "all" ? ev.seq : ev.stream_seq), ""),
        cell(formatTime(ev.received_at), "col-time"),
        cell(ev.type || "", "col-type"),
        cell(ev.subject || "", "col-subject"),
        cell(ev.id || "", "col-id")
      );
      var open = function () { selectEvent(ev.id); };
      tr.addEventListener("click", open);
      tr.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          open();
        }
      });
      return tr;
    }));
  }

  function cell(text, className) {
    var td = document.createElement("td");
    if (className) td.className = className;
    td.textContent = text;
    return td;
  }

  function selectEvent(id) {
    state.selectedId = id;
    var url = new URL(location.href);
    if (id) url.searchParams.set("event", id);
    else url.searchParams.delete("event");
    history.replaceState(null, "", url);
    renderEvents();
    syncDetail();
  }

  function syncDetail() {
    var ev = null;
    for (var i = 0; i < state.events.length; i++) {
      if (state.events[i].id === state.selectedId) {
        ev = state.events[i];
        break;
      }
    }
    if (!ev) {
      els.copy.hidden = true;
      if (!state.selectedId) {
        els.detail.textContent = "选择一条事件查看原文";
        return;
      }
      if (state.detailLoading === state.selectedId) return;
      state.detailLoading = state.selectedId;
      els.detail.textContent = "正在读取…";
      api("/v1/events/" + encodeURIComponent(state.selectedId)).then(function (body) {
        if (state.selectedId !== body.id) return;
        state.events.push(body);
        renderEvents();
        els.detail.textContent = JSON.stringify(body, null, 2);
        els.copy.hidden = false;
      }).catch(function (err) {
        if (err && err.status === 401) {
          explain(err);
          return;
        }
        if (state.detailLoading !== state.selectedId) return;
        els.detail.textContent = "这条事件不在当前窗口里。";
      });
      return;
    }
    els.detail.textContent = JSON.stringify(ev, null, 2);
    els.copy.hidden = false;
  }

  function formatTime(iso) {
    if (!iso) return "";
    var d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    function p(n) { return String(n).padStart(2, "0"); }
    return p(d.getMonth() + 1) + "-" + p(d.getDate()) + " " + p(d.getHours()) + ":" + p(d.getMinutes()) + ":" + p(d.getSeconds());
  }
})();
