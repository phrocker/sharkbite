CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: interconnect-accumulo

clean: 
	$(RM) $(OBJS)


interconnect-accumulo:	$(OBJS)
	g++ -o interconnect-accumulo.so $(OBJS) $(LDFLAGS) -shared -fPIC
	$(RM) $(OBJS)
