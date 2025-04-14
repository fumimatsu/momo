// エラー表示用関数
function showError(message) {
  const errorDiv = document.getElementById('error-message');
  errorDiv.innerText = message;
  errorDiv.style.display = 'block';
}

// エラー表示をクリアする関数
function hideError() {
  const errorDiv = document.getElementById('error-message');
  errorDiv.innerText = "";
  errorDiv.style.display = "none";
}

const remoteVideo = document.getElementById('remote_video');
// 自動再生対策：muted を設定
remoteVideo.muted = true;
remoteVideo.controls = true;

// 自動再接続用の設定
let reconnecting = false;
let autoReconnectDelay = 3000; // 3秒後に再接続

function autoReconnect() {
  if (reconnecting) return;
  reconnecting = true;
  console.log("自動再接続を試みます…");
  showError("接続が切断されました。再接続中です…");
  // 既存の接続を閉じる
  disconnect();
  // 一定時間後に再接続
  setTimeout(() => {
    connect();
    reconnecting = false;
  }, autoReconnectDelay);
}

// video 要素のエラー／終了イベントを監視して再接続を試みる
remoteVideo.addEventListener('error', (event) => {
  console.error("リモートビデオ要素でエラーが発生しました:", event);
  showError("リモートビデオでエラーが発生しました: " + (event.error ? event.error.message : 'unknown error'));
  autoReconnect();
});
remoteVideo.addEventListener('ended', (event) => {
  console.log("リモートビデオ要素が終了しました。");
  showError("リモートビデオが終了しました。");
  autoReconnect();
});

// --- 接続先ホストの設定 ---
// URL のクエリパラメーター 'ws' または sessionStorage を利用してホストを取得する関数
function getWsHost() {
  let wsHost = null;
  const urlParams = new URLSearchParams(window.location.search);
  if (urlParams.has('ws')) {
    wsHost = urlParams.get('ws');
  }
  if (!wsHost) {
    wsHost = sessionStorage.getItem('wsHost');
    if (!wsHost) {
      wsHost = prompt("接続先の WebSocket サーバーのホストを入力してください (例: sdk-2.local)", "sdk-2.local");
      if (!wsHost) {
        wsHost = "sdk-2.local";
      }
    }
  }
  sessionStorage.setItem('wsHost', wsHost);
  return wsHost;
}

const wsHost = getWsHost();
const wsUrl = `ws://${wsHost}:8080/ws`;
console.log("WebSocket 接続先:", wsUrl);

const ws = new WebSocket(wsUrl);
ws.onopen = onWsOpen.bind();
ws.onerror = onWsError.bind();
ws.onmessage = onWsMessage.bind();

let peerConnection = null;
let dataChannel = null;
let candidates = [];
let hasReceivedSdp = false;

// WebSocket が OPEN になる前に ICE candidate を一時保存するキュー
let pendingIceCandidates = [];

// ICE サーバの設定
const iceServers = [{ 'urls': 'stun:stun.l.google.com:19302' }];
const peerConnectionConfig = { 'iceServers': iceServers };

// codec セレクトがないため、デフォルトは "H264"
const defaultCodec = "H264";

function onWsError(error) {
  console.error('ws onerror() ERROR:', error);
  showError("WebSocket エラー: " + (error.message || error));
}

function onWsOpen(event) {
  console.log('ws open()');
  pendingIceCandidates.forEach(candidate => {
    const message = JSON.stringify({ type: 'candidate', ice: candidate });
    ws.send(message);
  });
  pendingIceCandidates = [];
}

function onWsMessage(event) {
  console.log('ws onmessage() data:', event.data);
  const message = JSON.parse(event.data);
  if (message.type === 'offer') {
    console.log('Received offer ...');
    const offer = new RTCSessionDescription(message);
    console.log('offer:', offer);
    setOffer(offer);
  } else if (message.type === 'answer') {
    console.log('Received answer ...');
    const answer = new RTCSessionDescription(message);
    console.log('answer:', answer);
    setAnswer(answer);
  } else if (message.type === 'candidate') {
    console.log('Received ICE candidate ...');
    const candidate = new RTCIceCandidate(message.ice);
    console.log('candidate:', candidate);
    if (hasReceivedSdp) {
      addIceCandidate(candidate);
    } else {
      candidates.push(candidate);
    }
  } else if (message.type === 'close') {
    console.log('peer connection is closed ...');
  }
}

function connect() {
  console.group();
  if (!peerConnection) {
    console.log('make Offer');
    makeOffer();
  } else {
    console.warn('peer connection already exists.');
  }
  console.groupEnd();
}

function disconnect() {
  console.group();
  if (peerConnection) {
    if (peerConnection.iceConnectionState !== 'closed') {
      peerConnection.close();
      peerConnection = null;
      if (ws && ws.readyState === WebSocket.OPEN) {
        const message = JSON.stringify({ type: 'close' });
        ws.send(message);
      }
      console.log('sending close message');
      cleanupVideoElement(remoteVideo);
      return;
    }
  }
  console.log('peerConnection is closed.');
  console.groupEnd();
}

function drainCandidate() {
  hasReceivedSdp = true;
  candidates.forEach(candidate => {
    addIceCandidate(candidate);
  });
  candidates = [];
}

function addIceCandidate(candidate) {
  if (peerConnection) {
    peerConnection.addIceCandidate(candidate).catch(e => {
      console.error("ICE candidate 追加エラー:", e);
    });
  } else {
    console.error('PeerConnection does not exist!');
    showError("ICE candidate の追加に失敗しました: PeerConnection が存在しません");
  }
}

function sendIceCandidate(candidate) {
  if (ws.readyState !== WebSocket.OPEN) {
    console.warn("WebSocket がまだ OPEN 状態ではありません。候補をキューに追加します。現在の状態:", ws.readyState);
    pendingIceCandidates.push(candidate);
    return;
  }
  console.log('---sending ICE candidate ---');
  const message = JSON.stringify({ type: 'candidate', ice: candidate });
  console.log('sending candidate=' + message);
  ws.send(message);
}

function sendSdp(sessionDescription) {
  console.log('---sending sdp ---');
  const message = JSON.stringify(sessionDescription);
  console.log('sending SDP=' + message);
  ws.send(message);
}

function playVideo(element, stream) {
  element.srcObject = stream;
}

function prepareNewConnection() {
  const peer = new RTCPeerConnection(peerConnectionConfig);
  dataChannel = peer.createDataChannel("serial");

  // 通常ブラウザの場合：最初に空の MediaStream を生成し video 要素にセット
  let mediaStream = new MediaStream();
  playVideo(remoteVideo, mediaStream);
  peer.ontrack = (event) => {
    console.log('-- peer.ontrack()');
    // 各トラックに対して error / ended イベントを追加して異常終了時に自動再接続を試みる
    event.track.addEventListener('ended', () => {
      console.log("メディアトラックが終了しました。", event.track);
      showError("メディアトラックが終了しました。");
      autoReconnect();
    });
    event.track.addEventListener('error', (err) => {
      console.error("メディアトラックエラー:", err);
      showError("メディアトラックでエラーが発生しました: " + (err.message || err));
      autoReconnect();
    });
    mediaStream.addTrack(event.track);
    remoteVideo.play().then(() => {
      hideError();
    }).catch(err => {
      console.warn("remoteVideo.play() failed:", err);
      showError("remoteVideo の再生に失敗しました: " + err.message);
    });
  };

  peer.onicecandidate = (event) => {
    console.log('-- peer.onicecandidate()');
    if (event.candidate) {
      console.log(event.candidate);
      sendIceCandidate(event.candidate);
    } else {
      console.log('empty ice event');
    }
  };

  peer.oniceconnectionstatechange = () => {
    console.log('-- peer.oniceconnectionstatechange()');
    console.log('ICE connection Status has changed to ' + peer.iceConnectionState);
    if (peer.iceConnectionState === 'connected' || peer.iceConnectionState === 'completed') {
      hideError();
    } else if (peer.iceConnectionState === 'failed' || peer.iceConnectionState === 'disconnected') {
      showError("ICE 接続が " + peer.iceConnectionState + " 状態になりました");
      autoReconnect();
    }
  };

  // 受信専用 transceiver の追加
  peer.addTransceiver('video', { direction: 'recvonly' });
  peer.addTransceiver('audio', { direction: 'recvonly' });

  dataChannel.onmessage = function (event) {
    console.log("Got Data Channel Message:", new TextDecoder().decode(event.data));
  };

  return peer;
}

function browser() {
  const ua = window.navigator.userAgent.toLowerCase();
  if (ua.indexOf('edge') !== -1) {
    return 'edge';
  } else if (ua.indexOf('chrome') !== -1 && ua.indexOf('edge') === -1) {
    return 'chrome';
  } else if (ua.indexOf('safari') !== -1 && ua.indexOf('chrome') === -1) {
    return 'safari';
  } else if (ua.indexOf('opera') !== -1) {
    return 'opera';
  } else if (ua.indexOf('firefox') !== -1) {
    return 'firefox';
  }
  return;
}

function isSafari() {
  return browser() === 'safari';
}

async function makeOffer() {
  peerConnection = prepareNewConnection();
  try {
    const sessionDescription = await peerConnection.createOffer({
      offerToReceiveAudio: true,
      offerToReceiveVideo: true
    });
    console.log('createOffer() success, SDP=', sessionDescription.sdp);
    // デフォルト codec に応じた不要な codec 除外処理
    switch (defaultCodec) {
      case 'H264':
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP8');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP9');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'AV1');
        break;
      case 'VP8':
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'H264');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP9');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'AV1');
        break;
      case 'VP9':
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'H264');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP8');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'AV1');
        break;
      case 'AV1':
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'H264');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP8');
        sessionDescription.sdp = removeCodec(sessionDescription.sdp, 'VP9');
        break;
    }
    await peerConnection.setLocalDescription(sessionDescription);
    console.log('setLocalDescription() success');
    hideError();
    sendSdp(peerConnection.localDescription);
  } catch (error) {
    console.error('makeOffer() ERROR:', error);
    showError("makeOffer() エラー: " + error.message);
  }
}

async function makeAnswer() {
  console.log('sending Answer. Creating remote session description...');
  if (!peerConnection) {
    const msg = 'peerConnection DOES NOT exist!';
    console.error(msg);
    showError(msg);
    return;
  }
  try {
    const sessionDescription = await peerConnection.createAnswer();
    console.log('createAnswer() success');
    await peerConnection.setLocalDescription(sessionDescription);
    console.log('setLocalDescription() success');
    hideError();
    sendSdp(peerConnection.localDescription);
    drainCandidate();
  } catch (error) {
    console.error('makeAnswer() ERROR:', error);
    showError("makeAnswer() エラー: " + error.message);
  }
}

function setOffer(sessionDescription) {
  if (peerConnection) {
    const msg = 'peerConnection already exists!';
    console.error(msg);
    showError(msg);
  }
  const pc = prepareNewConnection();
  pc.onnegotiationneeded = async function () {
    try {
      await pc.setRemoteDescription(sessionDescription);
      console.log('setRemoteDescription(offer) success');
      hideError();
      makeAnswer();
    } catch (error) {
      console.error('setRemoteDescription(offer) ERROR:', error);
      showError("setRemoteDescription(offer) エラー: " + error.message);
    }
  };
}

async function setAnswer(sessionDescription) {
  if (!peerConnection) {
    const msg = 'peerConnection DOES NOT exist!';
    console.error(msg);
    showError(msg);
    return;
  }
  try {
    await peerConnection.setRemoteDescription(sessionDescription);
    console.log('setRemoteDescription(answer) success');
    hideError();
    drainCandidate();
  } catch (error) {
    console.error('setRemoteDescription(answer) ERROR:', error);
    showError("setRemoteDescription(answer) エラー: " + error.message);
  }
}

function cleanupVideoElement(element) {
  element.pause();
  element.srcObject = null;
}

// codec 除外用の再帰関数（Stack Overflow のコードを参考）
function removeCodec(orgsdp, codec) {
  const internalFunc = (sdp) => {
    const codecre = new RegExp('(a=rtpmap:(\\d*) ' + codec + '\\/90000\\r\\n)');
    const rtpmaps = sdp.match(codecre);
    if (!rtpmaps || rtpmaps.length <= 2) {
      return sdp;
    }
    const rtpmap = rtpmaps[2];
    let modsdp = sdp.replace(codecre, '');
    const rtcpre = new RegExp('(a=rtcp-fb:' + rtpmap + '.*\\r\\n)', 'g');
    modsdp = modsdp.replace(rtcpre, '');
    const fmtpre = new RegExp('(a=fmtp:' + rtpmap + '.*\\r\\n)', 'g');
    modsdp = modsdp.replace(fmtpre, '');
    const aptpre = new RegExp('(a=fmtp:(\\d*) apt=' + rtpmap + '\\r\\n)');
    const aptmaps = modsdp.match(aptpre);
    let fmtpmap = "";
    if (aptmaps && aptmaps.length >= 3) {
      fmtpmap = aptmaps[2];
      modsdp = modsdp.replace(aptpre, '');
      const rtppre = new RegExp('(a=rtpmap:' + fmtpmap + '.*\\r\\n)', 'g');
      modsdp = modsdp.replace(rtppre, '');
    }
    let videore = /(m=video.*\r\n)/;
    const videolines = modsdp.match(videore);
    if (videolines) {
      let videoline = videolines[0].slice(0, -2);
      const videoelems = videoline.split(" ");
      let modvideoline = videoelems[0];
      videoelems.forEach((videoelem, index) => {
        if (index === 0) return;
        if (videoelem == rtpmap || videoelem == fmtpmap) return;
        modvideoline += " " + videoelem;
      });
      modvideoline += "\r\n";
      modsdp = modsdp.replace(videore, modvideoline);
    }
    return internalFunc(modsdp);
  };
  return internalFunc(orgsdp);
}

// ヘルパー関数：WebSocket が OPEN になるまで待ってからコールバックを実行
function waitForWebSocketOpen(callback) {
  if (ws.readyState === WebSocket.OPEN) {
    callback();
  } else {
    setTimeout(() => { waitForWebSocketOpen(callback); }, 100);
  }
}

// 【追加】映像更新を監視して一定時間更新が無ければ再接続する処理
let lastFrameTime = performance.now();
if ('requestVideoFrameCallback' in remoteVideo) {
  remoteVideo.requestVideoFrameCallback(function frameCallback(now, metadata) {
    lastFrameTime = now;
    remoteVideo.requestVideoFrameCallback(frameCallback);
  });
} else {
  remoteVideo.addEventListener('timeupdate', () => {
    lastFrameTime = performance.now();
  });
}
setInterval(() => {
  if (!remoteVideo.paused && !remoteVideo.ended) {
    let now = performance.now();
    if (now - lastFrameTime > 9000) { // 9000ms＝9秒以上更新が無い
      console.warn("9秒間、映像の更新が検出されません。自動再接続を試みます。");
      showError("動画更新がありません。再接続しています...");
      autoReconnect();
    }
  }
}, 1000);

// ページ読み込み時の初期処理
window.addEventListener('load', () => {
  // フルスクリーン要求（ユーザー操作がないと失敗する場合があるため、エラーはログ出力）
  if (document.documentElement.requestFullscreen) {
    document.documentElement.requestFullscreen().catch(err => {
      console.warn("フルスクリーンモードのリクエストに失敗しました:", err);
      showError("フルスクリーンモードのリクエストに失敗しました: " + err.message);
    });
  }
  // WebSocket が OPEN になってから接続開始
  waitForWebSocketOpen(connect);
  // 定期的に接続状態をチェック（15秒ごと）
  const connectionCheckInterval = 15000;
  setInterval(() => {
    if (!peerConnection || (peerConnection.iceConnectionState !== 'connected' &&
                              peerConnection.iceConnectionState !== 'completed')) {
      console.log("接続に失敗しているため、ページをリロードします。");
      showError("接続に失敗しているため、ページをリロードします。");
      location.reload();
    }
  }, connectionCheckInterval);
});
