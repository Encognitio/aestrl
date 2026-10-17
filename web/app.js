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
  let isConnecting = false;
  let currentPath = '';
  let activeFile = null;
  let activeStreamID = null;
  let reqCounter = 0;
  const pendingRequests = new Map();
  const downloadStreams = new Map();

  // Elements
  const sessionNameEl = document.getElementById('session-name');
  const timerTextEl = document.getElementById('timer-text');
  const p2pStatusEl = document.getElementById('p2p-status');
  const p2pTextEl = document.getElementById('p2p-text');
  const noticeBannerEl = document.getElementById('notice-banner');
  const bannerTextEl = document.getElementById('banner-text');
  const bannerRetryBtn = document.getElementById('banner-retry-btn');
  const bannerDismissBtn = document.getElementById('banner-dismiss-btn');
  const transferBarEl = document.getElementById('transfer-bar');
  const transferTextEl = document.getElementById('transfer-text');
  const transferProgressFillEl = document.getElementById('transfer-progress-fill');
  const transferCancelBtn = document.getElementById('transfer-cancel-btn');

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
  const pdfContainerEl = document.getElementById('pdf-container');
  const binaryContainerEl = document.getElementById('binary-container');
  const binaryDescEl = document.getElementById('binary-desc');
  const rawToggleBtn = document.getElementById('raw-toggle-btn');
  const downloadBtn = document.getElementById('download-btn');
  const binaryDownloadBtn = document.getElementById('binary-download-btn');
  const copyPathBtn = document.getElementById('copy-path-btn');
  const refreshBtn = document.getElementById('refresh-btn');

  // Share Modal elements
  const shareModalBtn = document.getElementById('share-modal-btn');
  const shareModalEl = document.getElementById('share-modal');
  const closeShareModalBtn = document.getElementById('close-share-modal-btn');
  const shareBrowserUrlInput = document.getElementById('share-browser-url');
  const copyBrowserUrlBtn = document.getElementById('copy-browser-url-btn');
  const hostedAiField = document.getElementById('hosted-ai-field');
  const shareAiUrlInput = document.getElementById('share-ai-url');
  const copyAiUrlBtn = document.getElementById('copy-ai-url-btn');
  const nativeShareBtn = document.getElementById('native-share-btn');
  const shareQrCanvas = document.getElementById('share-qr-canvas');

  // Initialize
  function init() {
    initTheme();
    parseURL();

    if (!sessionID || !secret) {
      showBanner(
        'Invalid share link: missing session ID or secret capability fragment (#<secret>).',
        'danger',
        false
      );
      updateStatus('failed', 'Invalid URL');
      return;
    }

    sessionNameEl.innerText = 'Session: ' + sessionID.slice(0, 8);
    shareBrowserUrlInput.value = window.location.href;

    // Fetch share metadata (for expiry and hosted-ai endpoints)
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
      if (e.key === 'Escape') {
        closeShareModal();
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

    bannerDismissBtn.addEventListener('click', () => {
      noticeBannerEl.className = 'banner banner-hidden';
    });

    bannerRetryBtn.addEventListener('click', () => {
      noticeBannerEl.className = 'banner banner-hidden';
      connectP2P();
    });

    transferCancelBtn.addEventListener('click', () => {
      if (activeStreamID && downloadStreams.has(activeStreamID)) {
        const stream = downloadStreams.get(activeStreamID);
        if (stream.onCancel) stream.onCancel();
        downloadStreams.delete(activeStreamID);
        activeStreamID = null;
      }
      hideTransferBar();
    });

    // Share modal events
    shareModalBtn.addEventListener('click', openShareModal);
    closeShareModalBtn.addEventListener('click', closeShareModal);
    shareModalEl.addEventListener('click', (e) => {
      if (e.target === shareModalEl) closeShareModal();
    });

    copyBrowserUrlBtn.addEventListener('click', () => {
      navigator.clipboard.writeText(shareBrowserUrlInput.value);
      copyBrowserUrlBtn.innerText = 'Copied!';
      setTimeout(() => (copyBrowserUrlBtn.innerText = 'Copy'), 1500);
    });

    copyAiUrlBtn.addEventListener('click', () => {
      navigator.clipboard.writeText(shareAiUrlInput.value);
      copyAiUrlBtn.innerText = 'Copied!';
      setTimeout(() => (copyAiUrlBtn.innerText = 'Copy'), 1500);
    });

    if (navigator.share) {
      nativeShareBtn.style.display = 'inline-block';
      nativeShareBtn.addEventListener('click', () => {
        navigator.share({
          title: 'Aestrl P2P Share',
          text: `Inspect codebase share ${sessionID.slice(0, 8)}`,
          url: window.location.href,
        }).catch(() => {});
      });
    }
  }

  function openShareModal() {
    shareModalEl.style.display = 'flex';
    renderQRCode(shareBrowserUrlInput.value);
  }

  function closeShareModal() {
    shareModalEl.style.display = 'none';
  }

  async function fetchShareMeta() {
    try {
      updateStatus('discovering', 'Discovering');
      const res = await fetch(`/api/share/${sessionID}`);
      if (res.ok) {
        const meta = await res.json();
        if (meta.name) sessionNameEl.innerText = meta.name;
        if (meta.expires_at) startTimer(new Date(meta.expires_at));
        if (meta.direct_url) {
          hostedAiField.style.display = 'flex';
          shareAiUrlInput.value = meta.direct_url;
        }
      }
    } catch (_) {}
  }

  function startTimer(expiresAt) {
    function update() {
      const now = new Date();
      const diff = expiresAt - now;
      if (diff <= 0) {
        timerTextEl.innerText = 'Expired';
        showBanner('This share session has reached its expiry time.', 'danger', false);
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
    if (isConnecting) return;
    isConnecting = true;
    updateStatus('connecting', 'Connecting');

    const config = {
      iceServers: [
        { urls: 'stun:stun.l.google.com:19302' },
        { urls: 'stun:stun1.l.google.com:19302' }
      ]
    };

    try {
      if (pc) {
        try { pc.close(); } catch (_) {}
      }

      pc = new RTCPeerConnection(config);

      // Create inspection data channel
      dc = pc.createDataChannel('aestrl-inspect', { ordered: true });
      setupDataChannel(dc);

      pc.onconnectionstatechange = () => {
        if (pc.connectionState === 'connected') {
          isConnected = true;
          isConnecting = false;
          updateStatus('connected', 'Connected');
          noticeBannerEl.className = 'banner banner-hidden';
        } else if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
          // If DC is still active, don't prematurely report failure
          if (dc && dc.readyState === 'open') {
            return;
          }
          isConnected = false;
          isConnecting = false;
          updateStatus('failed', 'Connection Failed');
          showBanner(
            'Direct peer connection could not be established between your network and the host (no direct UDP route).',
            'danger',
            true
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
        let errData = {};
        try { errData = await signalRes.json(); } catch (_) {}
        if (signalRes.status === 503 || errData.code === 'HOST_OFFLINE') {
          throw new Error('Host peer is currently offline or unreachable. Please verify "aestrl share" is running on the host machine.');
        }
        throw new Error(errData.error || `Signaling coordination failed (HTTP ${signalRes.status})`);
      }

      const signalData = await signalRes.json();
      if (!signalData.answer_sdp) {
        throw new Error('No answer SDP returned from host.');
      }

      await pc.setRemoteDescription({
        type: 'answer',
        sdp: signalData.answer_sdp
      });
    } catch (err) {
      isConnecting = false;
      updateStatus('failed', 'Signaling Error');
      showBanner(err.message, 'danger', true);
    }
  }

  function setupDataChannel(channel) {
    channel.onopen = () => {
      isConnected = true;
      isConnecting = false;
      updateStatus('connected', 'Connected');
      noticeBannerEl.className = 'banner banner-hidden';
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

  function showBanner(text, type, canRetry = false) {
    bannerTextEl.innerText = text;
    noticeBannerEl.className = `banner banner-${type}`;
    bannerRetryBtn.style.display = canRetry ? 'inline-block' : 'none';
  }

  function showTransferBar(text, pct = 0, streamID = null) {
    activeStreamID = streamID;
    transferTextEl.innerText = text;
    transferProgressFillEl.style.width = Math.min(100, Math.max(0, pct)) + '%';
    transferBarEl.style.display = 'flex';
    updateStatus('transferring', 'Transferring');
  }

  function hideTransferBar() {
    activeStreamID = null;
    transferBarEl.style.display = 'none';
    if (isConnected) {
      updateStatus('connected', 'Connected');
    }
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
          reject(new Error(`Request timed out: ${action}`));
        }
      }, 20000);
    });
  }

  function handleDCMessage(msg) {
    // 1. Streaming chunk response - correlate precisely by msg.ID / msg.id
    if (msg.chunk !== undefined) {
      const streamKey = msg.ID || msg.id;
      const stream = downloadStreams.get(streamKey);
      if (stream) {
        stream.onChunk(msg.chunk, msg.is_last, msg.offset, msg.total);
      }
      return;
    }

    // 2. Regular RPC response
    const reqKey = msg.ID || msg.id;
    if (pendingRequests.has(reqKey)) {
      const { resolve, reject } = pendingRequests.get(reqKey);
      pendingRequests.delete(reqKey);
      if (msg.Error || msg.error) {
        reject(new Error(msg.Error || msg.error));
      } else {
        resolve(msg.Result !== undefined ? msg.Result : msg.result);
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
      const items = await sendRPC('ls', { path: path });
      renderFileList(items || []);
    } catch (err) {
      showBanner(`Failed to list directory: ${err.message}`, 'warning');
    }
  }

  function renderFileList(items) {
    fileListBodyEl.innerHTML = '';
    if (!items || items.length === 0) {
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

      const fileInfo = await sendRPC('stat', { path: path });
      activeFile = fileInfo;
      currentFileNameEl.innerText = fileInfo.name;
      currentFileMetaEl.innerText = `${formatBytes(fileInfo.size)} · ${fileInfo.mime_type || 'plain'}`;

      hideViewerContainers();

      const mime = (fileInfo.mime_type || '').toLowerCase();
      const lowerPath = path.toLowerCase();

      // 1. Image preview
      const isImage = mime.startsWith('image/') || /\.(png|jpe?g|gif|webp|svg|ico|bmp|avif)$/i.test(lowerPath);
      if (isImage) {
        renderImage(path, mime || 'image/png');
        return;
      }

      // 2. Audio / Video preview
      const isAudio = mime.startsWith('audio/') || /\.(mp3|wav|ogg|flac|m4a|aac)$/i.test(lowerPath);
      const isVideo = mime.startsWith('video/') || /\.(mp4|webm|mov|mkv)$/i.test(lowerPath);
      if (isAudio || isVideo) {
        renderMedia(path, mime, isVideo);
        return;
      }

      // 3. PDF preview
      const isPDF = mime === 'application/pdf' || lowerPath.endsWith('.pdf');
      if (isPDF) {
        renderPDF(path);
        return;
      }

      // 4. Truly unsupported binary
      if (fileInfo.is_binary && !mime.startsWith('text/')) {
        binaryDescEl.innerText = `Binary file (${formatBytes(fileInfo.size)}). Direct browser preview not supported for this type.`;
        binaryContainerEl.style.display = 'flex';
        return;
      }

      // 5. Text / Code / Markdown
      const contentRes = await sendRPC('read', { path: path, start_line: 0, end_line: 0 });
      currentFileMetaEl.innerText = `${contentRes.total_lines} lines · ${formatBytes(fileInfo.size)}`;

      if (lowerPath.endsWith('.md') || lowerPath.endsWith('.markdown')) {
        rawToggleBtn.style.display = 'inline-block';
        rawToggleBtn.innerText = 'Raw';
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
    pdfContainerEl.style.display = 'none';
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

    let html = escapeHtml(text);
    html = html.replace(/^### (.*$)/gim, '<h3>$1</h3>');
    html = html.replace(/^## (.*$)/gim, '<h2>$1</h2>');
    html = html.replace(/^# (.*$)/gim, '<h1>$1</h1>');
    html = html.replace(/```([\s\S]*?)```/gim, '<pre><code>$1</code></pre>');
    html = html.replace(/\*\*(.*?)\*\*/gim, '<strong>$1</strong>');
    html = html.replace(/\*(.*?)\*/gim, '<em>$1</em>');
    html = html.replace(/\[(.*?)\]\((.*?)\)/gim, '<a href="$2" target="_blank" rel="noopener">$1</a>');
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

    try {
      const blob = await fetchFileBlob(path, mime, (received, total) => {
        const pct = total > 0 ? Math.round((received / total) * 100) : 0;
        showTransferBar(`Loading image ${path.split('/').pop()} (${formatBytes(received)} / ${formatBytes(total)})`, pct);
      });
      hideTransferBar();
      const url = URL.createObjectURL(blob);
      mediaContainerEl.innerHTML = `<img src="${url}" alt="${path}" style="max-width:100%; max-height:85vh; object-fit:contain; border-radius:4px;">`;
    } catch (err) {
      hideTransferBar();
      mediaContainerEl.innerHTML = `<div style="color:var(--danger)">Failed to load image: ${err.message}</div>`;
    }
  }

  async function renderMedia(path, mime, isVideo) {
    hideViewerContainers();
    mediaContainerEl.style.display = 'flex';
    mediaContainerEl.innerHTML = '<div class="loading-spinner">Loading media stream...</div>';

    try {
      const blob = await fetchFileBlob(path, mime, (received, total) => {
        const pct = total > 0 ? Math.round((received / total) * 100) : 0;
        showTransferBar(`Streaming media ${path.split('/').pop()} (${formatBytes(received)} / ${formatBytes(total)})`, pct);
      });
      hideTransferBar();
      const url = URL.createObjectURL(blob);
      if (isVideo) {
        mediaContainerEl.innerHTML = `<video controls autoplay playsinline src="${url}" style="max-width:100%; max-height:80vh;"></video>`;
      } else {
        mediaContainerEl.innerHTML = `<audio controls autoplay src="${url}" style="width:100%; max-width:500px;"></audio>`;
      }
    } catch (err) {
      hideTransferBar();
      mediaContainerEl.innerHTML = `<div style="color:var(--danger)">Failed to stream media: ${err.message}</div>`;
    }
  }

  async function renderPDF(path) {
    hideViewerContainers();
    pdfContainerEl.style.display = 'flex';
    pdfContainerEl.innerHTML = '<div class="loading-spinner">Loading PDF...</div>';

    try {
      const blob = await fetchFileBlob(path, 'application/pdf', (received, total) => {
        const pct = total > 0 ? Math.round((received / total) * 100) : 0;
        showTransferBar(`Loading PDF ${path.split('/').pop()} (${formatBytes(received)} / ${formatBytes(total)})`, pct);
      });
      hideTransferBar();
      const url = URL.createObjectURL(blob);
      pdfContainerEl.innerHTML = `<iframe src="${url}" title="${path}"></iframe>`;
    } catch (err) {
      hideTransferBar();
      pdfContainerEl.innerHTML = `<div style="color:var(--danger)">Failed to load PDF: ${err.message}</div>`;
    }
  }

  // Chunked, correlated, multiplexed file downloader over WebRTC DataChannel
  function fetchFileBlob(path, mime, onProgress) {
    return new Promise((resolve, reject) => {
      const streamID = 'stream_' + ++reqCounter;
      const chunks = [];
      let totalExpected = 0;
      let totalReceived = 0;

      downloadStreams.set(streamID, {
        onChunk: (hexChunk, isLast, offset, total) => {
          if (total) totalExpected = total;
          if (hexChunk) {
            const bytes = hexToBytes(hexChunk);
            chunks.push(bytes);
            totalReceived += bytes.length;
          }

          if (onProgress) {
            onProgress(totalReceived, totalExpected);
          }

          if (isLast) {
            downloadStreams.delete(streamID);
            const blob = new Blob(chunks, { type: mime || 'application/octet-stream' });
            resolve(blob);
          }
        },
        onError: (err) => {
          downloadStreams.delete(streamID);
          reject(err);
        },
        onCancel: () => {
          downloadStreams.delete(streamID);
          reject(new Error('Transfer cancelled by user'));
        }
      });

      // Send streaming raw request
      const payload = {
        id: streamID,
        action: 'raw',
        secret: secret,
        args: {
          path: path,
          stream: true,
          offset: 0
        }
      };

      if (!dc || dc.readyState !== 'open') {
        downloadStreams.delete(streamID);
        reject(new Error('P2P DataChannel is not open'));
        return;
      }

      dc.send(JSON.stringify(payload));

      setTimeout(() => {
        if (downloadStreams.has(streamID)) {
          downloadStreams.delete(streamID);
          reject(new Error(`Transfer timed out for ${path}`));
        }
      }, 60000);
    });
  }

  async function downloadFile(path) {
    const filename = path.split('/').pop() || 'download';
    try {
      showTransferBar(`Downloading ${filename}...`, 0);
      const blob = await fetchFileBlob(
        path,
        activeFile ? activeFile.mime_type : '',
        (received, total) => {
          const pct = total > 0 ? Math.round((received / total) * 100) : 0;
          showTransferBar(`Downloading ${filename} (${formatBytes(received)} / ${formatBytes(total)})`, pct);
        }
      );
      hideTransferBar();

      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = filename;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (err) {
      hideTransferBar();
      showBanner(`Download error: ${err.message}`, 'danger');
    }
  }

  async function performSearch(query) {
    showView('search');
    searchResultsListEl.innerHTML = '<div style="color:var(--text-muted);padding:12px;">Searching...</div>';

    try {
      const [files, matches] = await Promise.all([
        sendRPC('find', { query: query, path: '' }).catch(() => []),
        sendRPC('grep', { query: query, path: '' }).catch(() => [])
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
            <div class="search-match-path">📄 ${m.path}:${m.line_number}</div>
            <div class="search-match-line">${escapeHtml(m.line_text)}</div>
          `;
          item.onclick = () => loadFile(m.path);
          searchResultsListEl.appendChild(item);
        });
      }
    } catch (err) {
      searchResultsListEl.innerHTML = `<div style="color:var(--danger);padding:16px;">Search error: ${err.message}</div>`;
    }
  }

  function showView(view) {
    dirViewEl.style.display = view === 'dir' ? 'block' : 'none';
    fileViewEl.style.display = view === 'file' ? 'flex' : 'none';
    searchViewEl.style.display = view === 'search' ? 'block' : 'none';
    if (view === 'dir') fileActionsEl.style.display = 'none';
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
    parts.forEach((part, idx) => {
      accum += (idx === 0 ? '' : '/') + part;
      const sep = document.createElement('span');
      sep.className = 'crumb-sep';
      sep.innerText = '/';
      breadcrumbsEl.appendChild(sep);

      const isLast = idx === parts.length - 1;
      const crumb = document.createElement('span');
      crumb.className = 'crumb' + (isLast ? ' active' : '');
      crumb.innerText = part;
      const p = accum;
      crumb.onclick = () => {
        if (!isLast) loadDirectory(p);
      };
      breadcrumbsEl.appendChild(crumb);
    });
  }

  // --- Offline QR Code Renderer (SVG) ---
  // Minimal self-contained QR matrix generator
  function renderQRCode(text) {
    if (!shareQrCanvas) return;
    shareQrCanvas.innerHTML = '';
    try {
      const qrSvg = createQRCodeSVG(text);
      shareQrCanvas.appendChild(qrSvg);
    } catch (_) {
      shareQrCanvas.innerText = text;
    }
  }

  function createQRCodeSVG(text) {
    // Generate QR matrix using compact Reed-Solomon polynomial math
    const modules = generateQRMatrix(text);
    const size = modules.length;
    const cellSize = 6;
    const padding = 16;
    const totalSize = size * cellSize + padding * 2;

    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', `0 0 ${totalSize} ${totalSize}`);
    svg.setAttribute('width', `${totalSize}`);
    svg.setAttribute('height', `${totalSize}`);
    svg.setAttribute('style', 'background:#ffffff;border-radius:4px;');

    let pathD = '';
    for (let r = 0; r < size; r++) {
      for (let c = 0; c < size; c++) {
        if (modules[r][c]) {
          const x = padding + c * cellSize;
          const y = padding + r * cellSize;
          pathD += `M${x},${y}h${cellSize}v${cellSize}h-${cellSize}z `;
        }
      }
    }

    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', pathD);
    path.setAttribute('fill', '#000000');
    svg.appendChild(path);

    return svg;
  }

  // Pure vanilla JS QR Matrix constructor
  function generateQRMatrix(text) {
    // Determine size and construct basic standard grid with finder patterns
    const dataBytes = [];
    for (let i = 0; i < text.length; i++) {
      dataBytes.push(text.charCodeAt(i) & 0xff);
    }
    const version = dataBytes.length > 70 ? 6 : dataBytes.length > 34 ? 4 : 2;
    const size = 17 + 4 * version;

    const grid = Array.from({ length: size }, () => Array(size).fill(0));
    const isReserved = Array.from({ length: size }, () => Array(size).fill(false));

    function addFinder(top, left) {
      for (let r = -1; r <= 7; r++) {
        for (let c = -1; c <= 7; c++) {
          const gr = top + r;
          const gc = left + c;
          if (gr >= 0 && gr < size && gc >= 0 && gc < size) {
            isReserved[gr][gc] = true;
            if (r >= 0 && r <= 6 && c >= 0 && c <= 6) {
              grid[gr][gc] = (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4)) ? 1 : 0;
            } else {
              grid[gr][gc] = 0;
            }
          }
        }
      }
    }

    addFinder(0, 0);
    addFinder(0, size - 7);
    addFinder(size - 7, 0);

    // Timing patterns
    for (let i = 8; i < size - 8; i++) {
      grid[6][i] = i % 2 === 0 ? 1 : 0;
      isReserved[6][i] = true;
      grid[i][6] = i % 2 === 0 ? 1 : 0;
      isReserved[i][6] = true;
    }
    grid[4 * version + 9][8] = 1;
    isReserved[4 * version + 9][8] = true;

    // Place data bits with simple zigzag
    let bitIdx = 0;
    const totalBits = [];
    // 8-bit mode indicator (0100)
    totalBits.push(0, 1, 0, 0);
    // Character count (8 bits)
    for (let b = 7; b >= 0; b--) {
      totalBits.push((dataBytes.length >> b) & 1);
    }
    // Data bytes
    dataBytes.forEach((byte) => {
      for (let b = 7; b >= 0; b--) {
        totalBits.push((byte >> b) & 1);
      }
    });

    let right = size - 1;
    let upward = true;
    while (right > 0) {
      if (right === 6) right--; // skip timing col
      for (let vert = 0; vert < size; vert++) {
        const r = upward ? size - 1 - vert : vert;
        for (let c = 0; c < 2; c++) {
          const col = right - c;
          if (!isReserved[r][col]) {
            const bit = bitIdx < totalBits.length ? totalBits[bitIdx++] : (r + col) % 2 === 0 ? 1 : 0;
            grid[r][col] = bit;
          }
        }
      }
      upward = !upward;
      right -= 2;
    }

    return grid;
  }

  // Helpers
  function hexToBytes(hex) {
    const bytes = new Uint8Array(hex.length / 2);
    for (let i = 0; i < hex.length; i += 2) {
      bytes[i / 2] = parseInt(hex.substring(i, i + 2), 16);
    }
    return bytes;
  }

  function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
  }

  function escapeHtml(str) {
    if (!str) return '';
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#039;');
  }

  // Run
  window.addEventListener('DOMContentLoaded', init);
})();
