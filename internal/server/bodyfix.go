// bodyfix.go 出站请求体的「安全修正」。
//
// 目前只有一件事：把过小的输出额度抬到下限。
//
// 为什么必须做（真号实测 2026-10-26，账号 7772869 / sn-sensenova-6-8-flash）：
//
//	max_tokens=16   → finish_reason=length，content 空，reasoning_tokens=16
//	max_tokens=512  → finish_reason=length，content 空，reasoning_tokens=512
//	max_tokens=768  → finish_reason=stop，content "收到"，reasoning_tokens=228
//	max_tokens=1024 → 同上，reasoning_tokens=206
//
// 也就是说**思考 token 算在 max_tokens 里**，客户端给 16/512 这种「只要几个字」
// 的额度时，思考会把预算吃光、正文为空。客户端看到的现象是「模型坏了」，
// 实际是预算不够。目录里的 reasoning 标记不可靠（sensenova 标 false 照样烧
// 200+ 思考 token），所以只能统一抬，不能按模型分档。
//
// 边界：没给字段不动（不发明额度）、非数字不动、非法 JSON 原样返回
// （上游客户端自己会报错，网关不该多一层改动）。
package server

import "encoding/json"

// defaultMinOutputTokens 是出站 max_tokens 的默认下限。
//
// 1024 而不是 512：实测 512 仍会被思考吃光。思考量随问题复杂度波动
// （简单问题 200-300，复杂问题更多），1024 留了 2-4 倍余量。
const defaultMinOutputTokens = 2048

// escalatedMaxTokens 是「思考吃光额度」时二次尝试的额度。
//
// 实测 sn-sensenova-6-8-flash 的思考量能到 4000+ 字（≈2000+ token），
// 1024 会被吃光；8192 给了足够余量，而它只是上限、不会凭空多花。
const escalatedMaxTokens = 8192

// stripTurnTitle 把请求体里的 title 字段删掉。
//
// 网关会把它转成 X-RACCOON-Title 头（用于积分流水对账），但**不能留在 body 里**：
// 真号实测 raccoon-8c4485 对未声明字段是严格校验的，多一个 title 直接
// HTTP 500 "Unexpected keyword argument 'title'"（sn-* 系列反而会忽略）。
// 删掉字段不改变语义，只让上游看到它认识的形状。
func stripTurnTitle(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["title"]; !ok {
		return body
	}
	delete(obj, "title")
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// raiseMaxTokens 把输出额度至少设到 target（用于「思考吃光」后的重试）。
//
// 只抬不降：客户端本来写了更大的额度就不要动它。
func raiseMaxTokens(body []byte, target int) []byte {
	if target <= 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	found, changed := false, false
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		raw, exists := obj[key]
		if !exists {
			continue
		}
		found = true
		if cur, ok := asInt64Any(raw); ok && cur >= int64(target) {
			continue
		}
		obj[key] = target
		changed = true
	}
	if !found {
		obj["max_tokens"] = target
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// liftMaxTokens 把过小的输出额度抬到 floor。
//
// 三种协议的字段名都认：chat 的 max_tokens / max_completion_tokens、
// responses 的 max_output_tokens（messages 入口在转换时已折成 max_tokens）。
func liftMaxTokens(body []byte, floor int) []byte {
	if floor <= 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := obj[key].(float64)
		if !ok || v <= 0 || v >= float64(floor) {
			continue
		}
		obj[key] = float64(floor)
		changed = true
	}
	if !changed {
		return body
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}
