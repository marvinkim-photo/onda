import { describe, expect, it } from "vitest";
import { messageChannel, type MessageNode } from "@onda/journey-model";
import {
  extractVariables,
  isProfileReference,
  renderTemplatePreview,
  staleVariables,
  templateVariables,
  unmappedVariables,
  withMessageChannel,
} from "./alimtalk-variables";

const BODY = "#{고객명}님, #{주문번호} 주문이 발송되었습니다. 문의는 #{고객명}님 마이페이지에서.";

describe("extractVariables", () => {
  it("#{} 표기를 등장 순서대로 중복 없이 뽑는다 (워커 template.go와 같은 규약)", () => {
    expect(extractVariables(BODY)).toEqual(["고객명", "주문번호"]);
  });

  it("치환자가 없으면 빈 목록이다", () => {
    expect(extractVariables("안녕하세요")).toEqual([]);
  });

  it("저니의 {{변수}} 표기는 알림톡 치환자가 아니다", () => {
    expect(extractVariables("{{first_name}}님")).toEqual([]);
  });

  it("이름 앞뒤 공백은 지운다", () => {
    expect(extractVariables("#{ 고객명 }")).toEqual(["고객명"]);
  });
});

describe("templateVariables", () => {
  it("동기화가 준 variables를 우선 믿는다", () => {
    expect(templateVariables({ content: BODY, variables: ["주문번호"] })).toEqual(["주문번호"]);
  });

  it("variables가 비면 본문에서 직접 뽑는다 (매핑 UI가 비지 않게)", () => {
    expect(templateVariables({ content: BODY, variables: [] })).toEqual(["고객명", "주문번호"]);
  });

  it("템플릿이 없으면 빈 목록이다", () => {
    expect(templateVariables(null)).toEqual([]);
    expect(templateVariables(undefined)).toEqual([]);
  });

  it("중복 선언은 한 번만 센다", () => {
    expect(templateVariables({ variables: ["a", "a", " a "] })).toEqual(["a"]);
  });
});

describe("unmappedVariables", () => {
  const vars = ["고객명", "주문번호"];

  it("값이 없는 치환자를 드러낸다 — 발송 시점에 전부 실패하기 때문", () => {
    expect(unmappedVariables(vars, { 고객명: "{{first_name}}" })).toEqual(["주문번호"]);
  });

  it("공백만 있는 값은 매핑되지 않은 것으로 본다", () => {
    expect(unmappedVariables(vars, { 고객명: "  ", 주문번호: "A1" })).toEqual(["고객명"]);
  });

  it("매핑이 아예 없으면 전부 미매핑이다", () => {
    expect(unmappedVariables(vars, undefined)).toEqual(vars);
  });

  it("전부 채우면 빈 목록이다", () => {
    expect(unmappedVariables(vars, { 고객명: "홍길동", 주문번호: "A1" })).toEqual([]);
  });
});

describe("staleVariables", () => {
  it("템플릿을 바꿔 쓸모없어진 매핑을 알려 준다", () => {
    expect(staleVariables(["고객명"], { 고객명: "홍길동", 옛변수: "x" })).toEqual(["옛변수"]);
  });
});

describe("renderTemplatePreview", () => {
  it("매핑 값을 본문에 끼운다", () => {
    expect(renderTemplatePreview("#{고객명}님", { 고객명: "홍길동" })).toBe("홍길동님");
  });

  it("값이 없는 치환자는 #{이름} 그대로 남겨 빈 곳을 보이게 한다", () => {
    expect(renderTemplatePreview(BODY, { 고객명: "홍길동" })).toBe(
      "홍길동님, #{주문번호} 주문이 발송되었습니다. 문의는 홍길동님 마이페이지에서.",
    );
  });

  it("{{프로필속성}} 값은 그대로 보여 준다 — 실제 값은 발송 시점에 정해진다", () => {
    expect(renderTemplatePreview("#{고객명}님", { 고객명: "{{first_name}}" })).toBe("{{first_name}}님");
  });
});

describe("isProfileReference", () => {
  it("{{속성}} 한 개만 프로필 참조로 본다", () => {
    expect(isProfileReference("{{first_name}}")).toBe(true);
    expect(isProfileReference("  {{ first_name }}  ")).toBe(true);
    expect(isProfileReference("{{first_name}}님")).toBe(false);
    expect(isProfileReference("홍길동")).toBe(false);
  });
});

describe("withMessageChannel", () => {
  const base: MessageNode = { id: "n1", type: "message", push: { title: "t", body: "b" } };

  it("채널을 바꾸면 나머지 채널 키가 남지 않는다 (정확히 하나 불변식)", () => {
    for (const channel of ["push", "email", "alimtalk"] as const) {
      const next = withMessageChannel(base, channel);
      expect(messageChannel(next), channel).toBe(channel);
      expect(Object.keys(next).filter((k) => k === "push" || k === "email" || k === "alimtalk")).toEqual([channel]);
    }
  });

  it("노드 id를 보존한다 — 그래프의 간선이 끊기면 안 된다", () => {
    expect(withMessageChannel(base, "alimtalk").id).toBe("n1");
  });

  it("알림톡으로 바꾸면 빈 발신프로필·템플릿 코드로 시작한다", () => {
    expect(withMessageChannel(base, "alimtalk").alimtalk).toEqual({ sender_id: "", template_code: "" });
  });

  it("같은 채널로 다시 바꾸면 입력한 내용이 유지된다", () => {
    const node: MessageNode = {
      id: "n2", type: "message", alimtalk: { sender_id: "s1", template_code: "T1", variables: { a: "1" } },
    };
    expect(withMessageChannel(node, "alimtalk").alimtalk).toEqual(node.alimtalk);
  });

  it("이메일에서 알림톡으로 갔다가 돌아오면 이메일 내용은 비어 있다 (남은 키를 신뢰하지 않는다)", () => {
    const email: MessageNode = { id: "n3", type: "message", email: { subject: "s", html: "<p/>" } };
    const back = withMessageChannel(withMessageChannel(email, "alimtalk"), "email");
    expect(back.email).toEqual({ subject: "", html: "" });
    expect(messageChannel(back)).toBe("email");
  });
});
