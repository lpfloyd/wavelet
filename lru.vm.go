// Copyright (c) 2019 Perlin
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package wavelet

import (
	"container/list"
	"sync"

	"github.com/perlin-network/life/exec"
)

type VMLRU struct {
	sync.Mutex

	size int

	elements map[[32]byte]*list.Element
	access   *list.List
}

type objectInfoVM struct {
	key [32]byte
	obj *exec.VirtualMachine
}

func NewVMLRU(size int) *VMLRU {
	return &VMLRU{
		size:     size,
		elements: make(map[[32]byte]*list.Element, size),
		access:   list.New(),
	}
}

func (l *VMLRU) Load(key [32]byte) (*exec.VirtualMachine, bool) {
	l.Lock()
	defer l.Unlock()

	elem, ok := l.elements[key]
	if !ok {
		return nil, false
	}

	l.access.MoveToFront(elem)

	return elem.Value.(*objectInfoVM).obj, ok
}

func (l *VMLRU) LoadOrPut(key [32]byte, val *exec.VirtualMachine) (*exec.VirtualMachine, bool) {
	l.Lock()
	defer l.Unlock()

	elem, ok := l.elements[key]

	if ok {
		val = elem.Value.(*objectInfoVM).obj
		l.access.MoveToFront(elem)
	} else {
		l.elements[key] = l.access.PushFront(&objectInfoVM{
			key: key,
			obj: val,
		})
		for len(l.elements) > l.size {
			back := l.access.Back()
			info := back.Value.(*objectInfoVM)
			delete(l.elements, info.key)
			l.access.Remove(back)
		}
	}

	return val, ok
}

func (l *VMLRU) Put(key [32]byte, val *exec.VirtualMachine) {
	l.Lock()
	defer l.Unlock()

	elem, ok := l.elements[key]

	if ok {
		elem.Value.(*objectInfoVM).obj = val
		l.access.MoveToFront(elem)
	} else {
		l.elements[key] = l.access.PushFront(&objectInfoVM{
			key: key,
			obj: val,
		})
		for len(l.elements) > l.size {
			back := l.access.Back()
			info := back.Value.(*objectInfoVM)
			delete(l.elements, info.key)
			l.access.Remove(back)
		}
	}
}

func (l *VMLRU) Remove(key [32]byte) {
	l.Lock()
	defer l.Unlock()

	elem, ok := l.elements[key]
	if ok {
		delete(l.elements, key)
		l.access.Remove(elem)
	}
}
