import type { ReactNode } from 'react';
import { kindLabel, languageName, pluralize } from '@/lib/format';
import type { Facets, VideoFilters } from '@/lib/types';

/**
 * Five selects would push results below the fold on a phone, so filters start closed
 * and open by themselves when one is applied. It works with no JavaScript.
 */
export function FilterDisclosure({ active, children }: { active: number; children: ReactNode }) {
  return (
    <details className="filters" open={active > 0 || undefined}>
      <summary>Filters{active > 0 ? ` (${pluralize(active, 'active', 'active')})` : ''}</summary>
      {children}
    </details>
  );
}

interface FieldProps {
  label: string;
  name: string;
  value: string | undefined;
  options: { value: string; label: string }[];
  onChange?: (name: string, value: string) => void;
}

function Field({ label, name, value, options, onChange }: FieldProps) {
  return (
    <label className="filter-bar__field">
      {label}
      <select
        name={name}
        defaultValue={value ?? ''}
        onChange={onChange ? (event) => onChange(name, event.target.value) : undefined}
      >
        <option value="">All</option>
        {options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  );
}

/**
 * The filter controls that make sense for this catalog. A filter with a single value
 * chooses nothing, so it shows only when it is the one currently applied.
 */
export function FilterFields({
  facets,
  filters,
  onChange,
}: {
  facets: Facets;
  filters: VideoFilters;
  onChange?: (name: string, value: string) => void;
}) {
  const asOptions = (values: string[]) => values.map((value) => ({ value, label: value }));
  const useful = (values: readonly string[], current: string | undefined) =>
    values.length > 1 || Boolean(current);

  return (
    <>
      {useful(facets.kinds, filters.kind) ? (
        <Field
          label="Type"
          name="kind"
          value={filters.kind}
          options={facets.kinds.map((kind) => ({ value: kind, label: kindLabel(kind) }))}
          onChange={onChange}
        />
      ) : null}
      {useful(facets.categories, filters.category) ? (
        <Field label="Category" name="category" value={filters.category} options={asOptions(facets.categories)} onChange={onChange} />
      ) : null}
      {useful(facets.speakers, filters.speaker) ? (
        <Field label="Speaker" name="speaker" value={filters.speaker} options={asOptions(facets.speakers)} onChange={onChange} />
      ) : null}
      {useful(facets.languages, filters.language) ? (
        <Field
          label="Language"
          name="language"
          value={filters.language}
          options={facets.languages.map((language) => ({ value: language, label: languageName(language) }))}
          onChange={onChange}
        />
      ) : null}
      {useful(facets.tags, filters.tag) ? (
        <Field label="Tag" name="tag" value={filters.tag} options={asOptions(facets.tags)} onChange={onChange} />
      ) : null}
    </>
  );
}
