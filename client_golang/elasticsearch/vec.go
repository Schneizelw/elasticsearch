// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package elasticsearch

import (
    "fmt"
    "sync"
    "time"
    "bytes"
    "io"
    "io/ioutil"
    "strconv"
    "net/http"
    "encoding/json"
    "github.com/cihub/seelog"
    "github.com/Schneizelw/elasticsearch/common/model"
    dto "github.com/Schneizelw/elasticsearch/client_model/go"
)

const (
    SUM       = "Sum"
    HELP      = "Help"
    TYPE      = "Type"
    VALUE     = "Value"
    COUNT     = "Count"
    FQNAME    = "FqName"
    TIMESTAMP = "Timestamp"
    QUANTILE_50 = "QUANTILE_50"
    QUANTILE_90 = "QUANTILE_90"
    QUANTILE_99 = "QUANTILE_99"
    METRIC_GAUGE   = "Gauge"
    METRIC_COUNTER = "Counter"
    METRIC_SUMMARY = "Summary"
    COUNTER_TYPE = 1
    GAUGE_TYPE   = 2
    SUMMARY_TYPE = 3
)

var lastValueMap = make(map[uint64]float64)

// metricVec is a Collector to bundle metrics of the same name that differ in
// their label values. metricVec is not used directly (and therefore
// unexported). It is used as a building block for implementations of vectors of
// a given metric type, like GaugeVec, CounterVec, SummaryVec, and HistogramVec.
// It also handles label currying. It uses basicMetricVec internally.
type metricVec struct {
    *metricMap

    curry []curriedLabelValue

    // hashAdd and hashAddByte can be replaced for testing collision handling.
    hashAdd     func(h uint64, s string) uint64
    hashAddByte func(h uint64, b byte) uint64
}

// newMetricVec returns an initialized metricVec.
func newMetricVec(desc *Desc, url string, newMetric func(lvs ...string) Metric) *metricVec {
    return &metricVec{
        metricMap: &metricMap{
            metrics:   map[uint64][]metricWithLabelValues{},
            url:       url,
            desc:      desc,
            newMetric: newMetric,
        },
        hashAdd:     hashAdd,
        hashAddByte: hashAddByte,
    }
}

// DeleteLabelValues removes the metric where the variable labels are the same
// as those passed in as labels (same order as the VariableLabels in Desc). It
// returns true if a metric was deleted.
//
// It is not an error if the number of label values is not the same as the
// number of VariableLabels in Desc. However, such inconsistent label count can
// never match an actual metric, so the method will always return false in that
// case.
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider Delete(Labels) as an
// alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
// See also the CounterVec example.
func (m *metricVec) DeleteLabelValues(lvs ...string) bool {
    h, err := m.hashLabelValues(lvs)
    if err != nil {
        return false
    }

    return m.metricMap.deleteByHashWithLabelValues(h, lvs, m.curry)
}

// Delete deletes the metric where the variable labels are the same as those
// passed in as labels. It returns true if a metric was deleted.
//
// It is not an error if the number and names of the Labels are inconsistent
// with those of the VariableLabels in Desc. However, such inconsistent Labels
// can never match an actual metric, so the method will always return false in
// that case.
//
// This method is used for the same purpose as DeleteLabelValues(...string). See
// there for pros and cons of the two methods.
func (m *metricVec) Delete(labels Labels) bool {
    h, err := m.hashLabels(labels)
    if err != nil {
        return false
    }

    return m.metricMap.deleteByHashWithLabels(h, labels, m.curry)
}

func (m *metricVec) curryWith(labels Labels) (*metricVec, error) {
    var (
        newCurry []curriedLabelValue
        oldCurry = m.curry
        iCurry   int
    )
    for i, label := range m.desc.variableLabels {
        val, ok := labels[label]
        if iCurry < len(oldCurry) && oldCurry[iCurry].index == i {
            if ok {
                return nil, fmt.Errorf("label name %q is already curried", label)
            }
            newCurry = append(newCurry, oldCurry[iCurry])
            iCurry++
        } else {
            if !ok {
                continue // Label stays uncurried.
            }
            newCurry = append(newCurry, curriedLabelValue{i, val})
        }
    }
    if l := len(oldCurry) + len(labels) - len(newCurry); l > 0 {
        return nil, fmt.Errorf("%d unknown label(s) found during currying", l)
    }

    return &metricVec{
        metricMap:   m.metricMap,
        curry:       newCurry,
        hashAdd:     m.hashAdd,
        hashAddByte: m.hashAddByte,
    }, nil
}

func (m *metricVec) getMetricWithLabelValues(lvs ...string) (Metric, error) {
    h, err := m.hashLabelValues(lvs)
    if err != nil {
        return nil, err
    }

    return m.metricMap.getOrCreateMetricWithLabelValues(h, lvs, m.curry), nil
}

func (m *metricVec) getMetricWith(labels Labels) (Metric, error) {
    h, err := m.hashLabels(labels)
    if err != nil {
        return nil, err
    }

    return m.metricMap.getOrCreateMetricWithLabels(h, labels, m.curry), nil
}

func (m *metricVec) hashLabelValues(vals []string) (uint64, error) {
    if err := validateLabelValues(vals, len(m.desc.variableLabels)-len(m.curry)); err != nil {
        return 0, err
    }

    var (
        h             = hashNew()
        curry         = m.curry
        iVals, iCurry int
    )
    for i := 0; i < len(m.desc.variableLabels); i++ {
        if iCurry < len(curry) && curry[iCurry].index == i {
            h = m.hashAdd(h, curry[iCurry].value)
            iCurry++
        } else {
            h = m.hashAdd(h, vals[iVals])
            iVals++
        }
        h = m.hashAddByte(h, model.SeparatorByte)
    }
    return h, nil
}

func (m *metricVec) hashLabels(labels Labels) (uint64, error) {
    if err := validateValuesInLabels(labels, len(m.desc.variableLabels)-len(m.curry)); err != nil {
        return 0, err
    }

    var (
        h      = hashNew()
        curry  = m.curry
        iCurry int
    )
    for i, label := range m.desc.variableLabels {
        val, ok := labels[label]
        if iCurry < len(curry) && curry[iCurry].index == i {
            if ok {
                return 0, fmt.Errorf("label name %q is already curried", label)
            }
            h = m.hashAdd(h, curry[iCurry].value)
            iCurry++
        } else {
            if !ok {
                return 0, fmt.Errorf("label name %q missing in label map", label)
            }
            h = m.hashAdd(h, val)
        }
        h = m.hashAddByte(h, model.SeparatorByte)
    }
    return h, nil
}

// metricWithLabelValues provides the metric and its label values for
// disambiguation on hash collision.
type metricWithLabelValues struct {
    values []string
    metric Metric
}

// curriedLabelValue sets the curried value for a label at the given index.
type curriedLabelValue struct {
    index int
    value string
}

// metricMap is a helper for metricVec and shared between differently curried
// metricVecs.
type metricMap struct {
    mtx       sync.RWMutex // Protects metrics.
    metrics   map[uint64][]metricWithLabelValues
    url       string
    desc      *Desc
    newMetric func(labelValues ...string) Metric
}

func goRequest(url, data string) error {
    req, _ := http.NewRequest("PUT", url, bytes.NewReader([]byte(data)))
    req.Header.Set("Content-Type", "application/json;charset=UTF-8")
    client := http.Client{}
    res, err := client.Do(req)
    if err != nil {
        return err
    }
    if res != nil {
        io.Copy(ioutil.Discard, res.Body)
        res.Body.Close()
    }
    return nil
}

func setMetricData(metricType int,  dtoMetric dto.Metric, docMap map[string]interface{}) {
    switch metricType {
    case COUNTER_TYPE:
        dtoCounter := dtoMetric.GetCounter()
        docMap[TYPE] = METRIC_COUNTER
        docMap[VALUE] = dtoCounter.GetValue()
    case GAUGE_TYPE:
        dtoGauge := dtoMetric.GetGauge()
        docMap[TYPE] = METRIC_GAUGE
        docMap[VALUE] = dtoGauge.GetValue()
    case SUMMARY_TYPE:
        dtoSummary := dtoMetric.GetSummary()
        docMap[TYPE] = METRIC_SUMMARY
        docMap[SUM] = dtoSummary.GetSampleSum()
        docMap[COUNT] = dtoSummary.GetSampleCount()
        dtoQuantiles := dtoSummary.GetQuantile()
        for _, dtoQuantile := range dtoQuantiles {
            quantile := dtoQuantile.GetQuantile()
            value := dtoQuantile.GetValue()
            if quantile == 0.5 {
                docMap[QUANTILE_50] = value
            } else if quantile == 0.9 {
                docMap[QUANTILE_90] = value
            } else {
                docMap[QUANTILE_99] = value
            }
        }
    default:
        //do nothing
    }
}

func (m *metricMap) pushDocToEs(metricType int, metricLog seelog.LoggerInterface) {
    docMap := make(map[string]interface{}, len(m.desc.variableLabels))
    var url string
    var curValue float64
    timestamp := time.Now().UTC().Format(time.RFC3339)
    for hashValue, lvsSlice := range m.metrics {
        for _, lvs := range lvsSlice {
            for index, label := range m.desc.variableLabels {
                docMap[label] = lvs.values[index]
            }
            dtoMetric := dto.Metric{}
            if err := lvs.metric.Write(&dtoMetric); err != nil {
                continue
            }
            docMap[FQNAME] = m.desc.fqName
            docMap[HELP] = m.desc.help
            docMap[TIMESTAMP] = timestamp
            setMetricData(metricType, dtoMetric, docMap)
            if metricType == COUNTER_TYPE {
                curValue = docMap[VALUE].(float64)
                docMap[VALUE] = curValue - lastValueMap[hashValue]
                lastValueMap[hashValue] = curValue
            }
            data, err := json.Marshal(docMap)
            if err != nil {
                continue
            }
            url = m.url + strconv.Itoa(int(time.Now().UnixNano()))
            //fmt.Println(url, string(data))
            if err := goRequest(url, string(data)); err != nil {
                metricLog.Warn(err)
            }
        }
    }
}

// Describe implements Collector. It will send exactly one Desc to the provided
// channel.
func (m *metricMap) Describe(ch chan<- *Desc) {
    ch <- m.desc
}

// Collect implements Collector.
func (m *metricMap) Collect(ch chan<- Metric) {
    m.mtx.RLock()
    defer m.mtx.RUnlock()

    for _, metrics := range m.metrics {
        for _, metric := range metrics {
            ch <- metric.metric
        }
    }
}

// Reset deletes all metrics in this vector.
func (m *metricMap) Reset() {
    m.mtx.Lock()
    defer m.mtx.Unlock()

    for h := range m.metrics {
        delete(m.metrics, h)
    }
}

// deleteByHashWithLabelValues removes the metric from the hash bucket h. If
// there are multiple matches in the bucket, use lvs to select a metric and
// remove only that metric.
func (m *metricMap) deleteByHashWithLabelValues(
    h uint64, lvs []string, curry []curriedLabelValue,
) bool {
    m.mtx.Lock()
    defer m.mtx.Unlock()

    metrics, ok := m.metrics[h]
    if !ok {
        return false
    }

    i := findMetricWithLabelValues(metrics, lvs, curry)
    if i >= len(metrics) {
        return false
    }

    if len(metrics) > 1 {
        m.metrics[h] = append(metrics[:i], metrics[i+1:]...)
    } else {
        delete(m.metrics, h)
    }
    return true
}

// deleteByHashWithLabels removes the metric from the hash bucket h. If there
// are multiple matches in the bucket, use lvs to select a metric and remove
// only that metric.
func (m *metricMap) deleteByHashWithLabels(
    h uint64, labels Labels, curry []curriedLabelValue,
) bool {
    m.mtx.Lock()
    defer m.mtx.Unlock()

    metrics, ok := m.metrics[h]
    if !ok {
        return false
    }
    i := findMetricWithLabels(m.desc, metrics, labels, curry)
    if i >= len(metrics) {
        return false
    }

    if len(metrics) > 1 {
        m.metrics[h] = append(metrics[:i], metrics[i+1:]...)
    } else {
        delete(m.metrics, h)
    }
    return true
}

// getOrCreateMetricWithLabelValues retrieves the metric by hash and label value
// or creates it and returns the new one.
//
// This function holds the mutex.
func (m *metricMap) getOrCreateMetricWithLabelValues(
    hash uint64, lvs []string, curry []curriedLabelValue,
) Metric {
    m.mtx.RLock()
    metric, ok := m.getMetricWithHashAndLabelValues(hash, lvs, curry)
    m.mtx.RUnlock()
    if ok {
        return metric
    }

    m.mtx.Lock()
    defer m.mtx.Unlock()
    metric, ok = m.getMetricWithHashAndLabelValues(hash, lvs, curry)
    if !ok {
        inlinedLVs := inlineLabelValues(lvs, curry)
        metric = m.newMetric(inlinedLVs...)
        m.metrics[hash] = append(m.metrics[hash], metricWithLabelValues{values: inlinedLVs, metric: metric})
    }
    return metric
}

// getOrCreateMetricWithLabelValues retrieves the metric by hash and label value
// or creates it and returns the new one.
//
// This function holds the mutex.
func (m *metricMap) getOrCreateMetricWithLabels(
    hash uint64, labels Labels, curry []curriedLabelValue,
) Metric {
    m.mtx.RLock()
    metric, ok := m.getMetricWithHashAndLabels(hash, labels, curry)
    m.mtx.RUnlock()
    if ok {
        return metric
    }

    m.mtx.Lock()
    defer m.mtx.Unlock()
    metric, ok = m.getMetricWithHashAndLabels(hash, labels, curry)
    if !ok {
        lvs := extractLabelValues(m.desc, labels, curry)
        metric = m.newMetric(lvs...)
        m.metrics[hash] = append(m.metrics[hash], metricWithLabelValues{values: lvs, metric: metric})
    }
    return metric
}

// getMetricWithHashAndLabelValues gets a metric while handling possible
// collisions in the hash space. Must be called while holding the read mutex.
func (m *metricMap) getMetricWithHashAndLabelValues(
    h uint64, lvs []string, curry []curriedLabelValue,
) (Metric, bool) {
    metrics, ok := m.metrics[h]
    if ok {
        if i := findMetricWithLabelValues(metrics, lvs, curry); i < len(metrics) {
            return metrics[i].metric, true
        }
    }
    return nil, false
}

// getMetricWithHashAndLabels gets a metric while handling possible collisions in
// the hash space. Must be called while holding read mutex.
func (m *metricMap) getMetricWithHashAndLabels(
    h uint64, labels Labels, curry []curriedLabelValue,
) (Metric, bool) {
    metrics, ok := m.metrics[h]
    if ok {
        if i := findMetricWithLabels(m.desc, metrics, labels, curry); i < len(metrics) {
            return metrics[i].metric, true
        }
    }
    return nil, false
}

// findMetricWithLabelValues returns the index of the matching metric or
// len(metrics) if not found.
func findMetricWithLabelValues(
    metrics []metricWithLabelValues, lvs []string, curry []curriedLabelValue,
) int {
    for i, metric := range metrics {
        if matchLabelValues(metric.values, lvs, curry) {
            return i
        }
    }
    return len(metrics)
}

// findMetricWithLabels returns the index of the matching metric or len(metrics)
// if not found.
func findMetricWithLabels(
    desc *Desc, metrics []metricWithLabelValues, labels Labels, curry []curriedLabelValue,
) int {
    for i, metric := range metrics {
        if matchLabels(desc, metric.values, labels, curry) {
            return i
        }
    }
    return len(metrics)
}

func matchLabelValues(values []string, lvs []string, curry []curriedLabelValue) bool {
    if len(values) != len(lvs)+len(curry) {
        return false
    }
    var iLVs, iCurry int
    for i, v := range values {
        if iCurry < len(curry) && curry[iCurry].index == i {
            if v != curry[iCurry].value {
                return false
            }
            iCurry++
            continue
        }
        if v != lvs[iLVs] {
            return false
        }
        iLVs++
    }
    return true
}

func matchLabels(desc *Desc, values []string, labels Labels, curry []curriedLabelValue) bool {
    if len(values) != len(labels)+len(curry) {
        return false
    }
    iCurry := 0
    for i, k := range desc.variableLabels {
        if iCurry < len(curry) && curry[iCurry].index == i {
            if values[i] != curry[iCurry].value {
                return false
            }
            iCurry++
            continue
        }
        if values[i] != labels[k] {
            return false
        }
    }
    return true
}

func extractLabelValues(desc *Desc, labels Labels, curry []curriedLabelValue) []string {
    labelValues := make([]string, len(labels)+len(curry))
    iCurry := 0
    for i, k := range desc.variableLabels {
        if iCurry < len(curry) && curry[iCurry].index == i {
            labelValues[i] = curry[iCurry].value
            iCurry++
            continue
        }
        labelValues[i] = labels[k]
    }
    return labelValues
}

func inlineLabelValues(lvs []string, curry []curriedLabelValue) []string {
    labelValues := make([]string, len(lvs)+len(curry))
    var iCurry, iLVs int
    for i := range labelValues {
        if iCurry < len(curry) && curry[iCurry].index == i {
            labelValues[i] = curry[iCurry].value
            iCurry++
            continue
        }
        labelValues[i] = lvs[iLVs]
        iLVs++
    }
    return labelValues
}
