import type { ReactNode } from 'react';

export function EmptyState({
  title,
  children,
  actions,
  headingLevel: Heading = 'h2',
}: {
  title: string;
  children?: ReactNode;
  actions?: ReactNode;
  headingLevel?: 'h1' | 'h2';
}) {
  return (
    <section className="state">
      <Heading>{title}</Heading>
      {children ? <p>{children}</p> : null}
      {actions ? <div className="state__actions">{actions}</div> : null}
    </section>
  );
}
