(() => {
  'use strict';

  const id = 'experience-v1';
  const viewBox = '0 0 760 800';
  const pathD = `M476 144
    C555 144 617 177 646 222
    C672 263 681 347 686 424
    C690 491 690 561 687 619
    C685 657 659 682 625 697
    C584 715 541 704 510 688
    C486 676 473 658 478 638
    C484 616 506 596 544 578
    C579 562 599 544 604 514
    C610 477 607 430 597 398
    C588 369 566 353 542 352
    C520 351 502 365 480 380
    C459 395 442 397 419 390
    C391 382 367 365 341 348
    C315 331 292 322 272 327
    C247 333 229 351 218 377
    C207 403 211 433 224 458
    C239 486 264 504 292 513
    C318 520 345 519 365 529
    C390 542 403 566 403 591
    C403 617 389 638 366 653
    C341 670 308 674 274 673
    C222 672 182 657 158 630
    C134 603 132 563 134 513
    C136 458 137 398 139 341
    L142 286
    C145 239 159 206 183 182
    C210 155 249 148 287 148
    C350 149 421 143 476 144
    Z`;
  const sectorBoundaries = Object.freeze([0, 0.32346, 0.70503, 1]);
  const boundaries = Object.freeze({
    start: Object.freeze({ progress: 0, line: Object.freeze([476, 119, 476, 169]), label: Object.freeze([476, 105, 'START']) }),
    s1: Object.freeze({ progress: sectorBoundaries[1], line: Object.freeze([532, 555, 555, 595]), label: Object.freeze([507, 548, 'S1']) }),
    s2: Object.freeze({ progress: sectorBoundaries[2], line: Object.freeze([274, 648, 274, 698]), label: Object.freeze([274, 730, 'S2']) }),
  });

  function applyBoundary(svg, key, boundary) {
    const group = svg.querySelector(`[data-course-boundary="${key}"]`);
    if (!group) return;
    group.dataset.courseProgress = String(boundary.progress);
    for (const line of group.querySelectorAll('line')) {
      ['x1', 'y1', 'x2', 'y2'].forEach((attribute, index) => {
        line.setAttribute(attribute, String(boundary.line[index]));
      });
    }
    const label = group.querySelector('text');
    if (label) {
      label.setAttribute('x', String(boundary.label[0]));
      label.setAttribute('y', String(boundary.label[1]));
      label.textContent = boundary.label[2];
    }
  }

  function applyToSvg(svg) {
    if (!svg) throw new Error('Course layout SVG is required.');
    svg.setAttribute('viewBox', viewBox);
    svg.dataset.courseLayout = id;
    for (const path of svg.querySelectorAll('[data-course-path]')) {
      path.setAttribute('d', pathD);
    }
    for (const [key, boundary] of Object.entries(boundaries)) {
      applyBoundary(svg, key, boundary);
    }
    return svg;
  }

  window.MomoCourseLayout = Object.freeze({
    id,
    viewBox,
    pathD,
    sectorBoundaries,
    boundaries,
    applyToSvg,
  });
})();
