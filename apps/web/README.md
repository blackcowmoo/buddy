# Page composition

Every screen uses the same viewport shell and one scrolling content region.
Keep navigation, spacing, and section structure in the shared components;
feature pages own their data and interactions.

| Component | Responsibility |
| --- | --- |
| `AppShell` | Viewport grid, header, content, optional composer, keyboard skip action |
| `PageHeader` | Page identity, contextual actions, and the shared menu trigger/panel |
| `LearningPage` | Combines the shell, learning header, and scrolling `PageContent` |
| `LearningIntro` | Introduction and optional learning steps on a list/entry screen |
| `PageToolbar` | Labeled group of primary actions, with wrapping on narrow screens |
| `PageSection` | Named content region with a heading, description, and optional actions |
| `BackButton` | Return to a list from a detail or exercise view |
| `EmptyState`, `LoadingHint` | Reusable empty and loading feedback |

A learning page follows this order:

```tsx
<LearningPage title="학습" viewKey={selected?.id ?? "list"}>
  {selected ? <>
    <BackButton onClick={backToList} />
    <PageSection title={selected.title} className="page-card">
      {/* Feature-specific detail or exercise */}
    </PageSection>
  </> : <>
    <LearningIntro eyebrow="꾸준한 연습" title="오늘의 학습" description="학습 방법을 설명해요." />
    <PageToolbar>
      <button type="button" onClick={start}>새 학습 시작</button>
    </PageToolbar>
    <PageSection title="나의 학습 기록" description="최근 기록부터 보여요.">
      {/* LoadingHint, EmptyState, or DatedList/HistoryItem */}
    </PageSection>
  </>}
</LearningPage>
```

- Add destinations to `src/lib/navigation.ts`; home cards and menus consume
  the same labels, icons, descriptions, and order. Register the page in
  `lib/route.ts` and `main.tsx` without duplicating navigation markup.
- Keep the top-level home link in the hamburger menu. Detail return actions
  belong before detail content. Exercise actions remain beside their inputs.
- Use `PageContent` for the home screen and `LearningPage` for learning pages.
  Do not add another `main`, viewport height, or scrolling list inside them.
  Chat uses the same `AppShell` with its own transcript and composer.
- Change `viewKey` when navigating to a different view. Background polling
  and history pagination must keep that key stable to preserve scroll position.
- Keep links relative to the deployment root, including PR previews. The skip
  action moves focus without changing the URL hash used for room navigation.
- Use the shared layout and theme tokens in `styles.css`. Give flex/grid
  children `min-width: 0`, allow long text to wrap, and constrain popovers to
  their available width. Use `page-card` for a surfaced detail section. Keep
  header and composer outside the scroll region.
- Cover new behavior with deterministic tests, including list/detail return,
  primary actions, loading/error/empty states, and persisted progress where
  relevant. Check narrow screens, long text, keyboard navigation, and both themes.
