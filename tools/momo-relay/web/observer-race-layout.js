// Shared TeamObserver race layout. Static markup only; data is set through textContent.
export function mountObserverRaceLayout(root) {
  root.innerHTML = `<main class="race-grid">
      <aside class="leaderboard" aria-labelledby="leaderboardTitle">
        <div class="panel-heading">
          <div>
            <span class="eyebrow">ALL PARTICIPANTS</span>
            <h1 id="leaderboardTitle">RACE ORDER</h1>
          </div>
          <span id="panelCount" class="panel-count">0/4 VIDEO</span>
        </div>
        <div id="leaderboardRows" class="leaderboard-rows"></div>
      </aside>

      <section class="track-panel" aria-labelledby="trackMapTitle">
        <div class="panel-heading track-heading">
          <div>
            <span class="eyebrow">CONFIGURED COURSE</span>
            <h2 id="trackMapTitle">COURSE REFERENCE</h2>
          </div>
        </div>
        <div class="track-stage">
          <svg id="trackMap" class="track-map" viewBox="0 0 760 800" role="img" aria-label="Configured clockwise experience course">
            <defs>
              <filter id="markerGlow" x="-80%" y="-80%" width="260%" height="260%">
                <feGaussianBlur stdDeviation="6" result="blur"></feGaussianBlur>
                <feMerge><feMergeNode in="blur"></feMergeNode><feMergeNode in="SourceGraphic"></feMergeNode></feMerge>
              </filter>
            </defs>
            <g aria-label="Main course, clockwise">
              <path class="track-shadow" data-course-path></path>
              <path class="track-road" data-course-path></path>
              <path id="coursePath" class="track-line" data-course-path pathLength="1000"></path>
            </g>

            <g class="course-boundaries" aria-label="Course sector boundaries">
              <g class="course-boundary boundary-start" data-course-boundary="start">
                <line class="boundary-shadow"></line><line class="boundary-line"></line><text></text>
              </g>
              <g class="course-boundary boundary-s1" data-course-boundary="s1">
                <line class="boundary-shadow"></line><line class="boundary-line"></line><text></text>
              </g>
              <g class="course-boundary boundary-s2" data-course-boundary="s2">
                <line class="boundary-shadow"></line><line class="boundary-line"></line><text></text>
              </g>
            </g>

            <g id="trackMarkers" class="track-marker-layer" aria-label="Estimated vehicle positions"></g>

          </svg>

          <div class="map-caption">
            <span><i class="map-dot"></i>LAP PACE ESTIMATE / CHECKPOINT CORRECTED</span>
            <strong>NO GPS / NO RACING LINE</strong>
          </div>
        </div>
        <section id="cameraFocusStage" class="camera-focus-stage" aria-label="Focused onboard camera" hidden></section>
      </section>

      <aside class="timing-panel" aria-label="Current race timing data">
        <section class="sector-progress" aria-labelledby="sectorProgressTitle">
          <div class="panel-heading compact-heading">
            <div>
              <span class="eyebrow">CURRENT STATUS</span>
              <h2 id="sectorProgressTitle">SECTOR PROGRESS</h2>
            </div>
            <span class="updated-label">UPDATED <strong id="updatedAgo">0.1s</strong></span>
          </div>
          <div class="sector-columns" aria-hidden="true">
            <span>CAR</span><span>CURRENT SECTOR</span><span>LIVE</span><span>LAST</span><span>BEST</span>
          </div>
          <div id="sectorRows" class="sector-rows"></div>
        </section>

        <section class="timing-history" aria-labelledby="timingHistoryTitle">
          <div class="panel-heading compact-heading history-heading">
            <div>
              <span class="eyebrow">LATEST COMPLETED LAPS</span>
              <h2 id="timingHistoryTitle">TIMING HISTORY</h2>
            </div>
            <span id="historyCount" class="history-count">WAITING</span>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>TIME</th><th>CAR</th><th>LAP</th><th>S1</th><th>S2</th><th>S3</th><th>LAP TIME</th></tr></thead>
              <tbody id="timingRows"></tbody>
            </table>
          </div>
        </section>
      </aside>
    </main>

    <section class="situation-strip" aria-labelledby="situationTitle">
      <div class="situation-title">
        <span class="eyebrow">AUTO ANALYSIS</span>
        <h2 id="situationTitle">RACE SITUATION</h2>
      </div>
      <div id="situationRows" class="situation-rows"></div>
    </section>`;
}
