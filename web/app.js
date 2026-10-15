/**
 * Aestrl Browser Client
 * Native WebRTC direct P2P file inspection & exploration.
 */

(function () {
  // State
  let sessionID = '';
  let secret = '';
  let pc = null;
  let dc = null;
  let isConnected = false;
  let currentPath = '';
  let activeFile = null;
  let reqCounter = 0;
  const pendingRequests = new Map();
  const downloadStreams = new Map();

  // Elements
  const sessionNameEl = document.getElementById('session-name');
  const timerTextEl = document.getElementById('timer-text');
  const p2pStatusEl = document.getElementById('p2p-status');
  const p2pTextEl = document.getElementById('p2p-text');
  const noticeBannerEl = document.getElementById('notice-banner');
  const treeContainerEl = document.getElementById('tree-container');
  const breadcrumbsEl = document.getElementById('breadcrumbs');
  const fileActionsEl = document.getElementById('file-actions');
  const dirViewEl = document.getElementById('dir-view');
  const fileViewEl = document.getElementById('file-view');
  const searchViewEl = document.getElementById('search-view');
  const fileListBodyEl = document.getElementById('file-list-body');
  const globalSearchEl = document.getElementById('global-search');
  const searchResultsListEl = document.getElementById('search-results-list');
  const searchCountEl = document.getElementById('search-count');
  const themeToggleEl = document.getElementById('theme-toggle');

  // Viewer elements
  const currentFileNameEl = document.getElementById('current-file-name');
  const currentFileMetaEl = document.getElementById('current-file-meta');
  const codeContainerEl = document.getElementById('code-container');
  const lineNumbersEl = document.getElementById('line-numbers');
  const codeBodyEl = document.getElementById('code-body');
  const markdownContainerEl = document.getElementById('markdown-container');
  const mediaContainerEl = document.getElementById('media-container');
  const binaryContainerEl = document.getElementById('binary-container');
  const rawToggleBtn = document.getElementById('raw-toggle-btn');
  const downloadBtn = document.getElementById('download-btn');
  const binaryDownloadBtn = document.getElementById('binary-download-btn');
  const copyPathBtn = document.getElementById('copy-path-btn');
  const refreshBtn = document.getElementById('refresh-btn');

  // Initialize
  function init() {
    initTheme();
    parseURL();

    if (!sessionID || !secret) {
      showBanner('Invalid share link: missing session ID or secret capability fragment (#<secret>)', 'danger');
      p2pTextEl.innerText = 'Failed';
      return;
    }

    sessionNameEl.innerText = 'Session: ' + sessionID.slice(0, 8);

    // Fetch share metadata (for expiry)
    fetchShareMeta();

    // Connect WebRTC P2P
    connectP2P();

    // Event listeners
    setupEvents();
  }

  function parseURL() {
    const parts = window.location.pathname.split('/').filter(Boolean);
    const sIndex = parts.indexOf('s');
    if (sIndex !== -1 && sIndex + 1 < parts.length) {
      sessionID = parts[sIndex + 1];
    }
    // Extract secret from hash
    if (window.location.hash) {
      const hashStr = window.location.hash.substring(1);
      if (hashStr.includes('&')) {
        const params = new URLSearchParams(hashStr);
        secret = params.get('secret') || hashStr.split('&')[0];
      } else {
        secret = hashStr;
      }
    }
  }

  function initTheme() {
    const savedTheme = localStorage.getItem('aestrl-theme');
    if (savedTheme) {
      document.body.className = savedTheme;
    } else if (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches) {
      document.body.className = 'theme-light';
    }
  }

  function setupEvents() {
    themeToggleEl.addEventListener('click', () => {
      if (document.body.classList.contains('theme-dark')) {
        document.body.className = 'theme-light';
        localStorage.setItem('aestrl-theme', 'theme-light');
      } else {
        document.body.className = 'theme-dark';
        localStorage.setItem('aestrl-theme', 'theme-dark');
      }
    });

    window.addEventListener('keydown', (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
        e.preventDefault();
        globalSearchEl.focus();
      }
    });

    let searchTimer = null;
    globalSearchEl.addEventListener('input', (e) => {
      const q = e.target.value.trim();
      clearTimeout(searchTimer);
      if (!q) {
        showView('dir');
        return;
      }
      searchTimer = setTimeout(() => performSearch(q), 300);
    });

    copyPathBtn.addEventListener('click', () => {
      if (activeFile) {
        navigator.clipboard.writeText(activeFile.path);
        copyPathBtn.innerText = 'Copied!';
        setTimeout(() => (copyPathBtn.innerText = 'Copy Path'), 1500);
      }
    });

    downloadBtn.addEventListener('click', () => {
      if (activeFile) downloadFile(activeFile.path);
    });
    binaryDownloadBtn.addEventListener('click', () => {
      if (activeFile) downloadFile(activeFile.path);
    });

    refreshBtn.addEventListener('click', () => {
      loadTree();
      loadDirectory(currentPath);
    });
  }

  async function fetchShareMeta() {
    try {
      const res = await fetch(`/api/share/${sessionID}`);
      if (res.ok) {
        const meta = await res.json();
        if (meta.name) sessionNameEl.innerText = meta.name;
        if (meta.expires_at) startTimer(new Date(meta.expires_at));
      }
    } catch (_) {}
  }

  function startTimer(expiresAt) {
    function update() {
      const now = new Date();
      const diff = expiresAt - now;
      if (diff <= 0) {
        timerTextEl.innerText = 'Expired';
        showBanner('This share session has expired.', 'danger');
        return;
      }
      const hrs = Math.floor(diff / 3600000);
      const mins = Math.floor((diff % 3600000) / 60000);
      const secs = Math.floor((diff % 60000) / 1000);
      timerTextEl.innerText = `${hrs > 0 ? hrs + 'h ' : ''}${mins}m ${secs}s`;
    }
    update();
    setInterval(update, 1000);
  }

  // WebRTC P2P Setup
  async function connectP2P() {
    updateStatus('connecting', 'Connecting P2P');

    const config = {
      iceServers: [
        { urls: 'stun:stun.l.google.com:19302' },
        { urls: 'stun:stun1.l.google.com:19302' }
      ]
    };

    try {
      pc = new RTCPeerConnection(config);

      // Create inspection data channel
      dc = pc.createDataChannel('aestrl-inspect', { ordered: true });
      setupDataChannel(dc);

      pc.onconnectionstatechange = () => {
        if (pc.connectionState === 'connected') {
          updateStatus('connected', 'Direct P2P');
        } else if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
          updateStatus('failed', 'P2P Failed');
          showBanner(
            'Direct WebRTC P2P connection failed. Direct network path could not be negotiated (no TURN relay).',
            'danger'
          );
        }
      };

      // Create SDP offer
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);

      // Wait a short moment for candidate gathering
      await new Promise((r) => setTimeout(r, 600));

      const offerPayload = {
        offer_sdp: pc.localDescription.sdp,
        secret: secret
      };

      // Send to signaling endpoint
      const signalRes = await fetch(`/s/${sessionID}/signal`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(offerPayload)
      });

      if (!signalRes.ok) {
        const errText = await signalRes.text();
        throw new Error(errText || 'Signaling failed');
      }

      const signalData = await signalRes.json();
      if (!signalData.answer_sdp) {
        throw new Error('No answer SDP returned from host');
      }

      await pc.setRemoteDescription({
        type: 'answer',
        sdp: signalData.answer_sdp
      });
    } catch (err) {
      updateStatus('failed', 'Signaling Error');
      showBanner(`Connection error: ${err.message}`, 'danger');
    }
  }

  function setupDataChannel(channel) {
    channel.onopen = () => {
      isConnected = true;
      updateStatus('connected', 'Direct P2P');
      loadTree();
      loadDirectory('');
    };

    channel.onclose = () => {
      isConnected = false;
      updateStatus('failed', 'Disconnected');
    };

    channel.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data);
        handleDCMessage(msg);
      } catch (e) {
        console.error('Failed to parse message:', e);
      }
    };
  }

  function updateStatus(state, text) {
    p2pTextEl.innerText = text;
    const ind = p2pStatusEl.querySelector('.status-indicator');
    ind.className = `status-indicator status-${state}`;
  }

  function showBanner(text, type) {
    noticeBannerEl.innerText = text;
    noticeBannerEl.className = `banner banner-${type}`;
  }

  // DataChannel RPC Dispatcher
  function sendRPC(action, args) {
    return new Promise((resolve, reject) => {
      if (!isConnected || !dc || dc.readyState !== 'open') {
        reject(new Error('DataChannel not connected'));
        return;
      }

      const id = 'req_' + ++reqCounter;
      pendingRequests.set(id, { resolve, reject });

      const payload = {
        id: id,
        action: action,
        secret: secret,
        args: args || {}
      };

      dc.send(JSON.stringify(payload));

      setTimeout(() => {
        if (pendingRequests.has(id)) {
          pendingRequests.delete(id);
          reject(new Error('Request timed out'));
        }
      }, 15000);
    });
  }

  function handleDCMessage(msg) {
    // Handle streaming download chunk
    if (msg.action === 'get_file' && msg.chunk !== undefined) {
      handleDownloadChunk(msg);
      return;
    }

    if (pendingRequests.has(msg.ID)) {
      const { resolve, reject } = pendingRequests.get(msg.ID);
      pendingRequests.delete(msg.ID);
      if (msg.Error) {
        reject(new Error(msg.Error));
      } else {
        resolve(msg.Result);
      }
    }
  }

  // File Operations
  async function loadTree() {
    try {
      const tree = await sendRPC('tree', { path: '', depth: 4 });
      renderTree(tree);
    } catch (_) {}
  }

  function renderTree(rootNode) {
    if (!rootNode) return;
    treeContainerEl.innerHTML = '';
    const rootEl = document.createElement('div');
    rootEl.className = 'tree-item active';
    rootEl.innerHTML = `<span class="tree-icon">📁</span><span>${rootNode.Name || 'root'}</span>`;
    rootEl.onclick = () => loadDirectory('');
    treeContainerEl.appendChild(rootEl);

    if (rootNode.children) {
      renderTreeNodes(rootNode.children, treeContainerEl, 1);
    }
  }

  function renderTreeNodes(children, parentEl, depth) {
    children.forEach((child) => {
      const el = document.createElement('div');
      el.className = 'tree-item';
      el.style.paddingLeft = `${depth * 12 + 8}px`;
      const icon = child.is_dir ? '📁' : '📄';
      el.innerHTML = `<span class="tree-icon">${icon}</span><span>${child.name}</span>`;
      el.onclick = () => {
        if (child.is_dir) {
          loadDirectory(child.path);
        } else {
          loadFile(child.path);
        }
      };
      parentEl.appendChild(el);
      if (child.children && child.children.length > 0) {
        renderTreeNodes(child.children, parentEl, depth + 1);
      }
    });
  }

  async function loadDirectory(path) {
    currentPath = path;
    showView('dir');
    updateBreadcrumbs(path);

    try {
      const items = await sendRPC('list_directory', { path: path });
      renderFileList(items || []);
    } catch (err) {
      showBanner(`Failed to list directory: ${err.message}`, 'warning');
    }
  }

  function renderFileList(items) {
    fileListBodyEl.innerHTML = '';
    if (items.length === 0) {
      fileListBodyEl.innerHTML = '<tr><td colspan="3" style="text-align:center;color:var(--text-dim);padding:24px;">Empty directory</td></tr>';
      return;
    }

    // Sort: directories first, then alphabetically
    items.sort((a, b) => {
      if (a.is_dir === b.is_dir) return a.name.localeCompare(b.name);
      return a.is_dir ? -1 : 1;
    });

    items.forEach((item) => {
      const row = document.createElement('tr');
      row.className = 'file-row';
      const icon = item.is_dir ? '📁' : '📄';
      const sizeStr = item.is_dir ? '-' : formatBytes(item.size);
      const dateStr = item.mod_time ? new Date(item.mod_time).toLocaleString() : '';

      row.innerHTML = `
        <td class="file-name-cell">
          <span>${icon}</span>
          <span style="font-weight: ${item.is_dir ? '600' : '400'}">${item.name}</span>
        </td>
        <td class="td-size">${sizeStr}</td>
        <td class="td-date">${dateStr}</td>
      `;

      row.onclick = () => {
        if (item.is_dir) {
          loadDirectory(item.path);
        } else {
          loadFile(item.path);
        }
      };

      fileListBodyEl.appendChild(row);
    });
  }

  async function loadFile(path) {
    try {
      showView('file');
      updateBreadcrumbs(path);
      fileActionsEl.style.display = 'flex';

      const fileInfo = await sendRPC('file_info', { path: path });
      activeFile = fileInfo;
      currentFileNameEl.innerText = fileInfo.name;
      currentFileMetaEl.innerText = `${formatBytes(fileInfo.size)} · ${fileInfo.mime_type || 'plain'}`;

      hideViewerContainers();

      // Check binary
      if (fileInfo.is_binary) {
        binaryContainerEl.style.display = 'flex';
        return;
      }

      // Check media / images
      const mime = fileInfo.mime_type || '';
      if (mime.startsWith('image/')) {
        renderImage(path, mime);
        return;
      }
      if (mime.startsWith('audio/') || mime.startsWith('video/')) {
        renderMedia(path, mime);
        return;
      }

      // Read text file
      const contentRes = await sendRPC('read_file', { path: path, start_line: 0, end_line: 0 });
      currentFileMetaEl.innerText = `${contentRes.total_lines} lines · ${formatBytes(fileInfo.size)}`;

      // Check markdown
      if (path.endsWith('.md') || path.endsWith('.markdown')) {
        rawToggleBtn.style.display = 'inline-block';
        rawToggleBtn.innerText = 'Preview';
        rawToggleBtn.onclick = () => toggleMarkdownRaw(contentRes.content);
        renderMarkdown(contentRes.content);
      } else {
        rawToggleBtn.style.display = 'none';
        renderCode(contentRes.content);
      }
    } catch (err) {
      showBanner(`Failed to read file: ${err.message}`, 'warning');
    }
  }

  function hideViewerContainers() {
    codeContainerEl.style.display = 'none';
    markdownContainerEl.style.display = 'none';
    mediaContainerEl.style.display = 'none';
    binaryContainerEl.style.display = 'none';
  }

  function renderCode(text) {
    hideViewerContainers();
    codeContainerEl.style.display = 'flex';

    const lines = text.split('\n');
    lineNumbersEl.innerHTML = '';
    const codeTag = codeBodyEl.querySelector('code');
    codeTag.innerText = text;

    let numHtml = '';
    for (let i = 1; i <= lines.length; i++) {
      numHtml += `<div>${i}</div>`;
    }
    lineNumbersEl.innerHTML = numHtml;
  }

  function renderMarkdown(text) {
    hideViewerContainers();
    markdownContainerEl.style.display = 'block';

    // Simple, clean safe markdown renderer
    let html = escapeHtml(text);
    // Headers
    html = html.replace(/^### (.*$)/gim, '<h3>$1</h3>');
    html = html.replace(/^## (.*$)/gim, '<h2>$1</h2>');
    html = html.replace(/^# (.*$)/gim, '<h1>$1</h1>');
    // Code blocks
    html = html.replace(/```([\s\S]*?)```/gim, '<pre><code>$1</code></pre>');
    // Bold / italic
    html = html.replace(/\*\*(.*?)\*\*/gim, '<strong>$1</strong>');
    html = html.replace(/\*(.*?)\*/gim, '<em>$1</em>');
    // Links
    html = html.replace(/\[(.*?)\]\((.*?)\)/gim, '<a href="$2" target="_blank" rel="noopener">$1</a>');
    // Line breaks
    html = html.replace(/\n\n/gim, '<p></p>');

    markdownContainerEl.innerHTML = html;
  }

  function toggleMarkdownRaw(text) {
    if (markdownContainerEl.style.display === 'none') {
      rawToggleBtn.innerText = 'Raw';
      renderMarkdown(text);
    } else {
      rawToggleBtn.innerText = 'Preview';
      renderCode(text);
    }
  }

  async function renderImage(path, mime) {
    hideViewerContainers();
    mediaContainerEl.style.display = 'flex';
    mediaContainerEl.innerHTML = '<div class="loading-spinner">Loading image...</div>';

    const blob = await fetchFileBlob(path, mime);
    const url = URL.createObjectURL(blob);
    mediaContainerEl.innerHTML = `<img src="${url}" alt="${path}">`;
  }

  async function renderMedia(path, mime) {
    hideViewerContainers();
    mediaContainerEl.style.display = 'flex';
    const blob = await fetchFileBlob(path, mime);
    const url = URL.createObjectURL(blob);

    if (mime.startsWith('audio/')) {
      mediaContainerEl.innerHTML = `<audio controls src="${url}"></audio>`;
    } else {
      mediaContainerEl.innerHTML = `<video controls src="${url}"></video>`;
    }
  }

  // Chunked backpressured file downloader over DataChannel
  function fetchFileBlob(path, mime) {
    return new Promise((resolve, reject) => {
      const streamID = 'stream_' + ++reqCounter;
      const chunks = [];

      downloadStreams.set(streamID, {
        onChunk: (hexChunk, isLast) => {
          const bytes = hexToBytes(hexChunk);
          chunks.push(bytes);
          if (isLast) {
            downloadStreams.delete(streamID);
            const blob = new Blob(chunks, { type: mime || 'application/octet-stream' });
            resolve(blob);
          }
        },
        onError: (err) => {
          downloadStreams.delete(streamID);
          reject(err);
        }
      });

      sendRPC('get_file', { path: path })
        .then((res) => {
          // If returned as direct ReadFileResult
          if (res && res.content) {
            resolve(new Blob([res.content], { type: mime || 'text/plain' }));
          }
        })
        .catch(reject);
    });
  }

  function handleDownloadChunk(msg) {
    for (const [id, stream] of downloadStreams) {
      stream.onChunk(msg.chunk, msg.is_last);
      break;
    }
  }

  async function downloadFile(path) {
    try {
      showBanner(`Downloading ${path}...`, 'warning');
      const blob = await fetchFileBlob(path, activeFile ? activeFile.mime_type : '');
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = path.split('/').pop() || 'download';
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
      showBanner(`Downloaded ${path}`, 'warning');
      setTimeout(() => (noticeBannerEl.style.display = 'none'), 2000);
    } catch (err) {
      showBanner(`Download failed: ${err.message}`, 'danger');
    }
  }

  async function performSearch(query) {
    showView('search');
    searchResultsListEl.innerHTML = '<div style="color:var(--text-muted);padding:12px;">Searching...</div>';

    try {
      const [files, matches] = await Promise.all([
        sendRPC('search_files', { query: query, path: '' }).catch(() => []),
        sendRPC('search_content', { query: query, path: '' }).catch(() => [])
      ]);

      const total = (files ? files.length : 0) + (matches ? matches.length : 0);
      searchCountEl.innerText = `${total} hits`;
      searchResultsListEl.innerHTML = '';

      if (total === 0) {
        searchResultsListEl.innerHTML = '<div style="color:var(--text-muted);padding:16px;">No matches found.</div>';
        return;
      }

      if (files) {
        files.forEach((f) => {
          const item = document.createElement('div');
          item.className = 'search-match-item';
          item.innerHTML = `
            <div class="search-match-path">📄 ${f.path}</div>
            <div class="search-match-line">File name match (${formatBytes(f.size)})</div>
          `;
          item.onclick = () => loadFile(f.path);
          searchResultsListEl.appendChild(item);
        });
      }

      if (matches) {
        matches.forEach((m) => {
          const item = document.createElement('div');
          item.className = 'search-match-item';
          item.innerHTML = `
            <div class="search-match-path">${m.path}:${m.line_number}</div>
            <div class="search-match-line">${escapeHtml(m.line_text)}</div>
          `;
          item.onclick = () => loadFile(m.path);
          searchResultsListEl.appendChild(item);
        });
      }
    } catch (err) {
      searchResultsListEl.innerHTML = `<div style="color:var(--danger);padding:12px;">Search failed: ${err.message}</div>`;
    }
  }

  function updateBreadcrumbs(path) {
    breadcrumbsEl.innerHTML = '';
    const rootCrumb = document.createElement('span');
    rootCrumb.className = 'crumb' + (!path ? ' active' : '');
    rootCrumb.innerText = 'root';
    rootCrumb.onclick = () => loadDirectory('');
    breadcrumbsEl.appendChild(rootCrumb);

    if (!path) return;

    const parts = path.split('/').filter(Boolean);
    let accum = '';
    parts.forEach((part, i) => {
      accum += (accum ? '/' : '') + part;
      const sep = document.createElement('span');
      sep.className = 'crumb-sep';
      sep.innerText = '/';
      breadcrumbsEl.appendChild(sep);

      const crumb = document.createElement('span');
      const isLast = i === parts.length - 1;
      crumb.className = 'crumb' + (isLast ? ' active' : '');
      crumb.innerText = part;
      const target = accum;
      crumb.onclick = () => {
        if (isLast && activeFile) {
          // already there
        } else {
          loadDirectory(target);
        }
      };
      breadcrumbsEl.appendChild(crumb);
    });
  }

  function showView(view) {
    dirViewEl.style.display = view === 'dir' ? 'block' : 'none';
    fileViewEl.style.display = view === 'file' ? 'flex' : 'none';
    searchViewEl.style.display = view === 'search' ? 'block' : 'none';
    if (view === 'dir') fileActionsEl.style.display = 'none';
  }

  function formatBytes(bytes) {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
  }

  function hexToBytes(hex) {
    const bytes = new Uint8Array(hex.length / 2);
    for (let i = 0; i < hex.length; i += 2) {
      bytes[i / 2] = parseInt(hex.substr(i, 2), 16);
    }
    return bytes;
  }

  function escapeHtml(text) {
    const div = document.createElement('div');
    div.innerText = text;
    return div.innerHTML;
  }

  // Global API exposing all 15 core operations over P2P DataChannel
  window.aestrl = {
    exec: sendRPC,
    ls: (path) => sendRPC('ls', { path: path || '' }),
    tree: (path, depth) => sendRPC('tree', { path: path || '', depth: depth || 3 }),
    stat: (path) => sendRPC('stat', { path }),
    find: (query, path) => sendRPC('find', { query, path: path || '' }),
    search: (query, path) => sendRPC('search', { query, path: path || '' }),
    grep: (query, path, isRegex) => sendRPC('grep', { query, path: path || '', is_regex: !!isRegex }),
    context: (query, budget) => sendRPC('context', { query, budget: budget || 4000 }),
    read: (path, start, end) => sendRPC('read', { path, start_line: start || 0, end_line: end || 0 }),
    outline: (path) => sendRPC('outline', { path }),
    raw: (path, offset, length) => sendRPC('raw', { path, offset: offset || 0, length: length || -1 }),
    symbol: (name) => sendRPC('symbol', { name }),
    refs: (name) => sendRPC('refs', { name }),
    deps: (path) => sendRPC('deps', { path }),
    compare: (p1, p2) => sendRPC('compare', { path1: p1, path2: p2 }),
    manifest: () => sendRPC('manifest', {}),
  };

  // Start on page load
  window.addEventListener('DOMContentLoaded', init);
})();
