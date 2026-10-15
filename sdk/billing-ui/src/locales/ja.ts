import type { BillingUiMessageBundle } from "../i18n/messages"

export const ja: BillingUiMessageBundle = {
  common: {
    cancel: "キャンセル",
    close: "閉じる",
    retry: "再試行",
    loading: "読み込み中...",
    previous: "前へ",
    next: "次へ",
    notAvailable: "—",
  },
  status: {
    active: "有効",
    pending: "保留中",
    past_due: "支払い遅延",
    canceled: "解約済み",
    cancel_scheduled: "終了予定",
    unknown: "不明",
    awaiting_method: "カード更新が必要",
    unverified: "確認中",
    succeeded: "支払い済み",
    failed: "失敗",
    refunded: "返金済み",
    partially_refunded: "一部返金",
    expired: "期限切れ",
    expiring_soon: "まもなく期限切れ",
    needs_attention: "要確認",
    default: "デフォルト",
  },
  interval: {
    every: {
      hour: { "1": "毎時", other: "{count}時間ごと" },
      day: { "1": "毎日", other: "{count}日ごと" },
      week: { "1": "毎週", other: "{count}週間ごと" },
    },
    per: {
      hour: { "1": "/時間", other: "/{count}時間" },
      day: { "1": "/日", other: "/{count}日" },
      week: { "1": "/週", other: "/{count}週間" },
    },
    access: {
      hour: { other: "{count}時間のアクセス" },
      day: { other: "{count}日間のアクセス" },
      week: { other: "{count}週間のアクセス" },
    },
    duration: {
      hour: { other: "{count}時間" },
      day: { other: "{count}日間" },
      week: { other: "{count}週間" },
    },
    once: "買い切り",
    permanent: "有効期限のないアクセス",
  },
  products: {
    empty: "これを利用できる商品はありません",
    once: "{amount}",
    rent: "{period}レンタル、{amount}",
    recurring: "{amount} {every}",
  },
  subscriptions: {
    title: "サブスクリプション",
    description: "ご利用中のプランと更新日です。",
    empty: "サブスクリプションはまだありません",
    emptyDescription: "登録したプランがここに表示されます。",
    browsePlans: "プランを見る",
    fallbackName: "サブスクリプション",
    renews: "{date}に更新",
    endsOn: "{date}まで利用可能",
    noMorePayments: "今後の支払いはありません",
    endedOn: "{date}に終了",
    pastDue:
      "お支払いに失敗しました。引き続き利用するにはカードを更新してください。",
    cancel: "解約",
    cancelLabel: "{name}を解約",
    resume: "再開",
    resumed: "サブスクリプションは継続されます。",
    manageAtProvider: "管理",
    updating: "更新中...",
    paidWith: "{brand} •••• {last4}",
    wallet: "Solana ウォレット",
    changeCard: "カードを変更",
    changeCardLabel: "{name} のカードを変更",
    changePlan: "プランを変更",
    changePlanLabel: "{name} のプランを変更",
  },
  resume: {
    chooseCard: "支払い方法を選択",
    savedCardUnavailable:
      "この保存済みカードは利用できません。別のカードを選択するか、追加してください。",
    title: "{name}を再開しますか？",
    description:
      "サブスクリプションは{date}に更新され、その時点で{price}がカードに請求されます。解約するまで請求期間ごとに請求されます。",
    descriptionNoDate:
      "サブスクリプションは更新され、解約するまで請求期間ごとにカードに請求されます。",
    keepCanceled: "解約のままにする",
    confirm: "サブスクリプションを再開",
  },
  changeCard: {
    title: "{name} の支払い方法",
    description: "今後の更新は選択したカードに請求されます。",
    none: "切り替えるには、支払い方法から別のカードを追加してください。",
    confirm: "このカードを使う",
    current: "現在",
    useDefault: "デフォルトのカード（{card}）",
    done: "支払いカードを更新しました。",
  },
  changePlan: {
    title: "{name} を変更",
    description:
      "アップグレードはすぐに、ダウングレードは次回の更新時に適用されます。",
    none: "現在、切り替えられる他のプランはありません。",
    chargeNow: "今 {amount}、{date} から {next}",
    chargeNowEstimate: "今 約 {amount}、{date} から {next}",
    atPeriodEnd: "今は請求なし。{date} に切り替わり、その後 {next}",
    confirm: "プランを変更",
    signing: "ウォレットを待っています...",
    confirming: "確認しています...",
    done: "{name} に変更しました。",
    scheduled: "{name} は {date} に開始します。",
    processing:
      "プラン変更を送信しました。決済サービスが確認すると適用されます。",
    actionRequired:
      "この変更には追加の手順が必要です。もう一度お試しください。",
  },
  renewal: {
    title: "サブスクリプションを更新",
    description:
      "サブスクリプションが終了しました。プランを更新すると再びご利用いただけます。",
    action: "今すぐ更新",
  },
  cancel: {
    title: "{name}を解約しますか？",
    description: "今後の支払いは停止します。購入済みのアクセスは維持されます。",
    revokeOnChain:
      "オンチェーンでも解約する（ウォレットの署名とネットワーク手数料が必要です）。",
    solanaDescription:
      "ウォレットでオンチェーンの解約に署名します。購入済みのアクセスは維持されます。",
    reasonLabel: "解約の理由を教えてください",
    reasonPlaceholder: "ご意見はサービス改善に役立てます。",
    reasonHint: "{count}/{max}",
    reasonTooShort: "{min}文字以上入力してください。",
    keep: "継続する",
    confirm: "解約する",
    confirmWallet: "ウォレットで署名",
    working: "解約中...",
    preparing: "トランザクションを準備中...",
    signing: "ウォレットで承認してください...",
    confirming: "解約を記録中...",
    done: "解約を受け付けました。",
  },
  paymentMethods: {
    title: "支払い方法",
    description: "サブスクリプションに使う保存済みカードです。",
    empty: "保存済みのカードはありません",
    emptyDescription: "保存したカードがここに表示されます。",
    add: "カードを追加",
    addTitle: "カードを追加",
    addDescription:
      "カード情報は決済代行会社に直接送信され、このサイトには届きません。",
    save: "カードを保存",
    added: "カードを保存しました。",
    provider: "決済プロバイダー",
    saveNotice:
      "入力したカードは今後のお支払いのためにアカウントに保存されます。",
    unavailable: "このプロバイダーでは安全なカード入力を利用できません。",
    addUnavailable: "現在、カードを追加できません。",
    verificationPending:
      "カードの確認が保留中です。もう一度試す前に保存済みのカードを確認してください。",
    cardLabel: "{brand} •••• {last4}",
    fallbackBrand: "カード",
    expires: "有効期限 {date}",
    usedBy: "{names}で使用中",
    defaultFor: "{currencies}のデフォルト",
    makeDefault: "デフォルトにする",
    defaultUpdated: "デフォルトのカードを更新しました。",
    remove: "削除",
    removeLabel: "{label}を削除",
    removeTitle: "{label}を削除しますか？",
    removeDescription:
      "有効なサブスクリプションで使用中のカードは、別のカードに切り替えてから削除してください。",
    removeConfirm: "カードを削除",
    removed: "カードを削除しました。",
    removeFailed:
      "カードの削除を確認できませんでした。もう一度お試しください。",
    inUse:
      "このカードは有効なサブスクリプションの支払いに使われています。先に別のカードへ切り替えてください。",
  },
  purchase: {
    renews: "更新：{period}",
  },
  history: {
    title: "支払い履歴",
    description: "アカウントの請求と返金です。",
    empty: "支払いはまだありません",
    emptyDescription: "完了した支払いがここに表示されます。",
    date: "日付",
    item: "内容",
    subscription: "サブスクリプション",
    purchase: "購入",
    method: "支払い方法",
    status: "状態",
    amount: "金額",
    refund: "返金",
    refundedAmount: "{amount} 返金済み",
    page: "{page}ページ",
    rail: {
      card: "カード",
      solana: "Solana",
      ccbill: "カード",
      nmi: "カード",
      stripe: "カード",
    },
  },
  errors: {
    generic: "問題が発生しました。もう一度お試しください。",
    network_error:
      "サーバーに接続できません。接続を確認してもう一度お試しください。",
    server_unavailable:
      "サーバーが現在応答していません。しばらくしてからもう一度お試しください。",
    invalid_response: "サーバーから予期しない応答がありました。",
    authentication_required:
      "セッションの有効期限が切れました。再度サインインしてください。",
    resource_access_denied: "アクセス権がありません。",
    resource_not_found: "この項目は存在しません。",
    resource_conflict:
      "内容が更新されています。再読み込みしてもう一度お試しください。",
    rate_limit_exceeded:
      "リクエストが多すぎます。しばらく待ってからお試しください。",
    provider_writes_disabled: "このサイトでは現在、支払いが停止されています。",
    service_unavailable:
      "請求機能は一時的に利用できません。しばらくしてからお試しください。",
    internal_error: "システムで問題が発生しました。もう一度お試しください。",
    card_declined: "カードが拒否されました。",
    payment_provider_rejected: "決済代行会社がリクエストを拒否しました。",
    provider_outcome_unknown:
      "プロバイダーで結果を確認できませんでした。しばらくしてから確認してください。",
    payment_in_progress: "すでに支払いを処理中です。",
    payment_method_delete_unsupported:
      "このカードはここでは削除できません。サポートにお問い合わせください。",
    payment_method_delete_failed:
      "プロバイダーがこのカードを削除できませんでした。後でもう一度お試しください。",
    customer_session_required: "この操作には再度サインインが必要です。",
    invoker_scoped_principal: "このアカウントでは請求を管理できません。",
    csrf_origin_denied:
      "リクエストがブロックされました。ページを再読み込みしてください。",
    wallet_rejected: "ウォレットのリクエストが拒否されました。",
    wallet_no_signature: "ウォレットがトランザクションに署名しませんでした。",
    subscription_change_in_flight:
      "このサブスクリプションのプラン変更はまだ処理中です。完了するまでお待ちください。",
    subscription_change_idempotency_conflict:
      "このプラン変更は別の内容ですでに送信されています。もう一度お試しください。",
    subscription_change_already_scheduled:
      "次回の更新時のプラン変更がすでに予定されています。",
    subscription_change_target_inactive:
      "このプランは現在ご利用いただけません。",
    subscription_not_active:
      "プランを変更できるのは有効なサブスクリプションのみです。",
  },
}
