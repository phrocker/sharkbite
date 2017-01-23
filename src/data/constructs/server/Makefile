CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: server

clean: 
	$(RM) $(OBJS)


server:	$(OBJS)
	g++ -o server.so $(OBJS) $(LDFLAGS) -shared -fPIC
	$(RM) $(OBJS)
