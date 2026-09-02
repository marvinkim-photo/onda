"use client";

import { useEffect, useRef, useState } from "react";
import type { ImportedEmailTemplate } from "./email-template-zip";
import { JourneyIcon } from "./journey-ui";
import "./journey-email-template-sheet.css";

interface Props {
  template: ImportedEmailTemplate;
  onCancel: () => void;
  onChooseAnother: () => void;
  onApply: () => void;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function JourneyEmailTemplateSheet({ template, onCancel, onChooseAnother, onApply }: Props) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [viewport, setViewport] = useState<"desktop" | "mobile">("desktop");
  const [previewKey, setPreviewKey] = useState(0);

  useEffect(() => {
    const element = dialog.current;
    if (!element) return;
    const scrollPosition = { left: window.scrollX, top: window.scrollY };
    const rootOverflow = document.documentElement.style.overflow;
    const bodyOverflow = document.body.style.overflow;
    document.documentElement.style.overflow = "hidden";
    document.body.style.overflow = "hidden";
    element.showModal();
    window.scrollTo(scrollPosition);
    const frame = window.requestAnimationFrame(() => window.scrollTo(scrollPosition));
    return () => {
      window.cancelAnimationFrame(frame);
      if (element.open) element.close();
      document.documentElement.style.overflow = rootOverflow;
      document.body.style.overflow = bodyOverflow;
    };
  }, []);

  return (
    <dialog ref={dialog} className="j-template-sheet" aria-labelledby="j-template-sheet-title"
      onCancel={(event) => { event.preventDefault(); onCancel(); }}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.preventDefault();
          onCancel();
        }
      }}>
      <header className="j-template-sheet-header">
        <h2 id="j-template-sheet-title" tabIndex={-1} autoFocus>템플릿 적용 전 미리보기</h2>
        <button type="button" className="j-template-icon-button" aria-label="미리보기 닫기" onClick={onCancel}>
          <JourneyIcon name="close" size={20} />
        </button>
      </header>

      <div className="j-template-sheet-body">
        <section className="j-template-file-summary" aria-label="업로드한 ZIP 정보">
          <div className="j-template-file-name">
            <span className="j-template-file-icon"><JourneyIcon name="message" size={17} /></span>
            <strong>{template.archiveName}</strong>
            <small>{formatBytes(template.archiveSize)}</small>
            <span className="j-template-success"><JourneyIcon name="check" size={14} />검사 완료</span>
          </div>
          <button type="button" className="j-template-secondary-button" onClick={onChooseAnother}>다른 ZIP 선택</button>
        </section>

        <section className="j-template-checks" aria-label="ZIP 검사 결과">
          <span><JourneyIcon name="check" size={15} />{template.entryPath} 확인</span>
          <span><JourneyIcon name="check" size={15} />외부 스크립트 없음</span>
          <span title={template.imageCount > 0 ? `로컬 이미지 ${template.imageCount}개를 HTML에 포함했습니다.` : undefined}>
            <JourneyIcon name="check" size={15} />이미지 경로 정리됨
          </span>
        </section>

        <details className="j-template-file-details">
          <summary>파일 {template.fileCount}개 보기</summary>
          <ul>{template.files.map((path) => <li key={path}>{path}</li>)}</ul>
        </details>

        {template.warnings.length > 0 && (
          <div className="j-template-warning" role="status"><JourneyIcon name="info" size={16} />
            <div><strong>미리보기에서 정리한 항목</strong>{template.warnings.map((warning) => <p key={warning}>{warning}</p>)}</div>
          </div>
        )}

        <section className="j-template-preview-section" aria-label="이메일 템플릿 미리보기">
          <div className="j-template-preview-toolbar">
            <div className="j-template-viewport-switch" role="group" aria-label="미리보기 너비">
              <button type="button" aria-pressed={viewport === "desktop"}
                className={viewport === "desktop" ? "is-active" : undefined} onClick={() => setViewport("desktop")}>Desktop</button>
              <button type="button" aria-pressed={viewport === "mobile"}
                className={viewport === "mobile" ? "is-active" : undefined} onClick={() => setViewport("mobile")}>Mobile</button>
            </div>
            <button type="button" className="j-template-refresh" onClick={() => setPreviewKey((key) => key + 1)}>
              새로고침
            </button>
          </div>
          <div className={`j-template-preview-stage is-${viewport}`}>
            <iframe key={previewKey} title={`${template.archiveName} 이메일 미리보기`} sandbox="" srcDoc={template.previewHtml} />
          </div>
        </section>
      </div>

      <footer className="j-template-sheet-footer">
        <p>적용 후에도 HTML을 직접 수정할 수 있습니다.</p>
        <div>
          <button type="button" className="j-button" onClick={onCancel}>취소</button>
          <button type="button" className="j-button j-button-primary" onClick={onApply}>이 템플릿 사용</button>
        </div>
      </footer>
    </dialog>
  );
}
