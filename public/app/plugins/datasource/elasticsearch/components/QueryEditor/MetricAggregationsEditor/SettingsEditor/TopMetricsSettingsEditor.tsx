import { AsyncMultiSelect, InlineField, SegmentAsync, Select } from '@grafana/ui';
import React, { FunctionComponent } from 'react';
import { useDispatch } from '../../../../hooks/useStatelessReducer';
import { TopMetrics } from '../aggregations';
import { changeMetricSetting } from '../state/actions';
import { SettingField } from './SettingField';
import { orderOptions } from '../../BucketAggregationsEditor/utils';
import { range } from 'lodash';
import { useFields } from 'app/plugins/datasource/elasticsearch/hooks/useFields';
import { css } from '@emotion/css';
import { SelectableValue } from '@grafana/data';

const aggregateByOptions = [
  { value: 'avg', label: 'Average' },
  { value: 'sum', label: 'Sum' },
  { value: 'max', label: 'Max' },
  { value: 'min', label: 'Min' },
  { value: 'concat', label: 'Concatenate' },
];

interface Props {
  metric: TopMetrics;
}

const toMultiSelectValue = (value: string): SelectableValue<string> => ({ value, label: value });

export const TopMetricsSettingsEditor: FunctionComponent<Props> = ({ metric }) => {
  const dispatch = useDispatch();
  const getOrderByOptions = useFields(['number', 'date', 'boolean']);
  const getMetricsOptions = useFields(metric.type);

  return (
    <>
      <InlineField label="Metrics" labelWidth={16}>
        <AsyncMultiSelect
          onChange={(e) =>
            dispatch(
              changeMetricSetting(
                metric,
                'metrics',
                e.map((v) => v.value!)
              )
            )
          }
          loadOptions={getMetricsOptions}
          value={metric.settings?.metrics?.map(toMultiSelectValue)}
          closeMenuOnSelect={false}
          defaultOptions
        />
      </InlineField>
      <InlineField label="Order" labelWidth={16}>
        <Select
          onChange={(e) => dispatch(changeMetricSetting(metric, 'order', e.value))}
          options={orderOptions}
          value={metric.settings?.order}
        />
      </InlineField>
      <InlineField
        label="Order By"
        labelWidth={16}
        className={css`
          & > div {
            width: 100%;
          }
        `}
      >
        <SegmentAsync
          className={css`
            margin-right: 0;
          `}
          loadOptions={getOrderByOptions}
          onChange={(e) => dispatch(changeMetricSetting(metric, 'orderBy', e.value))}
          placeholder="Select Field"
          value={metric.settings?.orderBy}
        />
      </InlineField>
      <InlineField label="Size" labelWidth={16}>
        <Select
          onChange={(e) => dispatch(changeMetricSetting(metric, 'size', e.value))}
          options={range(0, 10).map((value) => ({ value: `${value + 1}`, label: `${value + 1}` }))}
          value={metric.settings?.size}
        />
      </InlineField>
      <InlineField label="Aggregate by" labelWidth={16}>
        <Select
          onChange={(e) => dispatch(changeMetricSetting(metric, 'aggregateBy', e.value))}
          options={aggregateByOptions}
          value={metric.settings?.aggregateBy}
        />
      </InlineField>
      {metric.settings?.aggregateBy === 'concat' && (
        <SettingField label="Separator" metric={metric} settingName="separator" />
      )}
    </>
  );
};